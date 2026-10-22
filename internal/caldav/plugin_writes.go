// Calendar-object write validation (PUT interception) and RFC 5995 Add-Member.

package caldav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/pathutil"
)

// InterceptWrite implements dav.WriteInterceptor for calendar-object PUTs,
// mirroring sabre's validateICalendar: a jCal (RFC 7265) body is converted to
// iCalendar, and the object's component type is checked against the calendar's
// supported-calendar-component-set. Runs before the scheduling broker
// (registration order), so downstream sees validated iCalendar.
func (p *Plugin) InterceptWrite(r *http.Request, path string, data []byte, isNew bool) ([]byte, error) {
	parentPath, _ := pathutil.Split(path)
	parent, err := p.server.Tree.NodeForPath(r.Context(), parentPath)
	if err != nil {
		return data, nil // not a calendar object write; leave it to other handlers
	}
	calNode, isCal := parent.(*Calendar)
	if !isCal {
		return data, nil
	}

	// jCal → iCalendar. Malformed jCal is rejected rather than stored raw.
	if ical.IsJCal(data) {
		decoded, err := ical.DecodeJCal(data)
		if err != nil {
			return nil, dav.ErrBadRequest("Invalid jCal request body: %s", err)
		}
		data = ical.EncodeLenient(decoded)
	}

	// A body that will not parse as iCalendar is rejected up front, the way
	// sabre's validateICalendar does, rather than handed to the backend, which
	// would surface the failure as an opaque 500.
	cal, err := ical.Decode(data)
	if err != nil {
		return nil, dav.ErrUnsupportedMediaType(
			"This resource only supports valid iCalendar 2.0 data. Parse error: %s", err)
	}
	compType := ical.ComponentType(cal)
	if compType == "" {
		return nil, invalidComponentType("iCalendar object must contain a VEVENT, VTODO or VJOURNAL component")
	}
	if allowed := calNode.SupportedComponents(); !slices.Contains(allowed, compType) {
		return nil, invalidComponentType(fmt.Sprintf(
			"This calendar only supports %s; the object is a %s", strings.Join(allowed, ", "), compType))
	}
	// RFC 5545 §3.6.5: every TZID used needs a VTIMEZONE. Clients (calweb
	// among them) send bare IANA names; attach a definition generated from the
	// same zone data expansion uses (docs/dst-recurring-events.md §5). A zone
	// whose future rule can't be written faithfully rejects the PUT rather
	// than store an approximation.
	if p.AddVTimezones {
		withTZ, err := ical.AddMissingVTimezones(data, cal)
		if err != nil {
			return nil, invalidCalendarData("Cannot attach a time zone definition: %s", err)
		}
		data = withTZ
	}
	// The backend denormalizes every stored object (first/last occurrence,
	// recurrence bounds). Run that here so data which parses structurally but
	// is semantically invalid — a malformed DURATION, date, or RRULE — fails
	// as a 400 carrying the CalDAV valid-calendar-data precondition instead of
	// an opaque backend 500 leaking an internal error string.
	if _, err := ical.Denormalize(data); err != nil {
		return nil, invalidCalendarData("Invalid iCalendar data: %s", err)
	}
	return data, nil
}

// invalidCalendarData reports iCalendar that parses but is not usable (bad
// durations, dates, or recurrence rules) as a 400 carrying the RFC 4791
// {CALDAV:}valid-calendar-data precondition, mirroring sabre's precondition
// reporting rather than leaking an internal error as a 500.
func invalidCalendarData(format string, args ...any) error {
	e := dav.ErrBadRequest(format, args...)
	e.Precondition = []xml.Name{{Space: davxml.NSCalDAV, Local: "valid-calendar-data"}}
	return e
}

// invalidComponentType mirrors sabre's Sabre\CalDAV\Exception\InvalidComponentType
// (a 403 Forbidden subclass) so the error identity matches on the wire.
func invalidComponentType(msg string) error {
	return &dav.Error{
		Status:    http.StatusForbidden,
		Exception: `Sabre\CalDAV\Exception\InvalidComponentType`,
		Message:   msg,
	}
}

// addMemberSegment is the trailing path segment RFC 5995 Add-Member POSTs
// target (Calendar Server's convention).
const addMemberSegment = ";add-member"

// handleAddMember implements RFC 5995: a POST to "<calendar>/;add-member"
// stores the body as a new calendar object under a server-assigned name and
// returns 201 with its Location. Non-add-member POSTs (the scheduling outbox)
// fall through to the next plugin.
func (p *Plugin) handleAddMember(w http.ResponseWriter, r *http.Request) (bool, error) {
	path, err := p.server.Path(r)
	if err != nil {
		return false, nil
	}
	if !strings.HasSuffix(path, "/"+addMemberSegment) {
		return false, nil
	}
	calPath := strings.TrimSuffix(path, "/"+addMemberSegment)
	node, err := p.server.Tree.NodeForPath(r.Context(), calPath)
	if err != nil {
		return true, err
	}
	cal, isCal := node.(*Calendar)
	if !isCal {
		return true, dav.ErrForbidden("Add-Member is only supported on calendar collections")
	}
	// Add-Member creates a new object in the calendar, so it requires the same
	// {DAV:}bind privilege a PUT-create is gated by. POST is not covered by the
	// ACL beforeMethod table, so enforce it here — otherwise Add-Member is a
	// write-authorization bypass (any authenticated user writing into another
	// principal's calendar).
	checker, err := RequirePrivilegeChecker(p.server)
	if err != nil {
		return true, err
	}
	if err := checker.CheckPrivileges(r.Context(), calPath, "{DAV:}bind"); err != nil {
		return true, err
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return true, err
	}
	name := randomToken() + ".ics"
	targetPath := calPath + "/" + name

	// Run the same write-interceptor chain a PUT would (validation +
	// scheduling), so an Add-Member POST is not a validation bypass.
	for _, plugin := range p.server.Plugins() {
		if wi, ok := plugin.(dav.WriteInterceptor); ok {
			raw, err = wi.InterceptWrite(r, targetPath, raw, true)
			if err != nil {
				return true, err
			}
		}
	}
	etag, err := cal.CreateFile(r.Context(), name, bytes.NewReader(raw))
	if err != nil {
		return true, err
	}
	for _, plugin := range p.server.Plugins() {
		if hook, ok := plugin.(dav.AfterWriteHook); ok {
			hook.AfterWrite(r, targetPath)
		}
	}

	h := w.Header()
	h.Set("Location", p.server.BaseURI+targetPath)
	if etag != "" {
		h.Set("ETag", etag)
	}
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return true, nil
}
