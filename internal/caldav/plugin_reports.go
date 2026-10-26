// The calendar reports (calendar-query, multiget, free-busy-query) and calendar-data transforms.

package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"calendsd/internal/caldav/ical"
	"calendsd/internal/dav"
	davxml "calendsd/internal/dav/xml"
	"calendsd/internal/pathutil"
)

type freeBusyQueryXML struct {
	XMLName   xml.Name      `xml:"urn:ietf:params:xml:ns:caldav free-busy-query"`
	TimeRange *timeRangeXML `xml:"time-range"`
}

// PrivilegeChecker is the ACL enforcement surface the free-busy report needs
// (implemented by the acl plugin).
type PrivilegeChecker interface {
	CheckPrivileges(ctx context.Context, path string, privileges ...string) error
}

// RequirePrivilegeChecker resolves the registered ACL enforcement surface,
// failing closed when none is present. The free-busy REPORT, the Add-Member
// POST, the outbox schedule-send POST, and the sharing actions all enforce
// their own privilege because those methods sit outside the ACL plugin's
// BeforeMethod table. If the acl plugin were dropped or reordered out of the
// stack, iterating for a PrivilegeChecker and skipping when absent would turn
// each of those into an unauthenticated bypass, not a no-op — so a missing
// checker is a denial (and compose() also asserts one is wired at startup).
func RequirePrivilegeChecker(server *dav.Server) (PrivilegeChecker, error) {
	for checker := range dav.Hooks[PrivilegeChecker](server) {
		return checker, nil
	}
	return nil, dav.ErrForbidden("access control is not configured on this server")
}

// maxFreeBusySpan bounds the window of a free-busy-query REPORT and of an
// outbox VFREEBUSY request. Free-busy expands every recurring event of every
// readable calendar across the window, and the window is client-chosen (sabre
// defaults an open end to the year 3000), so without a bound one request
// over a few dense rules is an allocation storm. 366 days covers every client
// default (Apple asks for about a month, Thunderbird for six weeks) with room
// for a leap year. A wider window is refused, never silently truncated: a
// truncated answer looks complete and invites double bookings (audit 3.1,
// D1). Expressed in days so the message and the check agree.
const maxFreeBusySpanDays = 366

// freeBusyWindow validates a free-busy window against maxFreeBusySpanDays. An
// open-ended window is refused for the same reason as a wide one.
func freeBusyWindow(start, end time.Time) (time.Time, time.Time, error) {
	if start.IsZero() || end.IsZero() {
		return time.Time{}, time.Time{}, dav.ErrForbidden(
			"The free-busy time-range must specify both a start and an end, at most %d days apart", maxFreeBusySpanDays)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, dav.ErrBadRequest("The free-busy time-range end must be after its start")
	}
	if end.Sub(start) > maxFreeBusySpanDays*24*time.Hour {
		return time.Time{}, time.Time{}, dav.ErrForbidden(
			"The free-busy time-range may span at most %d days", maxFreeBusySpanDays)
	}
	return start, end, nil
}

// freeBusyQueryReport implements the free-busy-query REPORT (RFC 4791
// §7.10), answering 200 with a text/calendar VFREEBUSY body like sabre.
func (p *Plugin) freeBusyQueryReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	var req freeBusyQueryXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	if req.TimeRange == nil {
		return dav.ErrBadRequest("The free-busy-query REPORT requires a time-range element")
	}
	tr, err := convertTimeRange(req.TimeRange)
	if err != nil {
		return err
	}
	start, end, err := freeBusyWindow(tr.Start, tr.End)
	if err != nil {
		return err
	}

	// Free-busy is gated by its own privilege ({DAV:}authenticated holds
	// it via the calendar ACL).
	checker, err := RequirePrivilegeChecker(p.server)
	if err != nil {
		return err
	}
	if err := checker.CheckPrivileges(r.Context(), path,
		"{urn:ietf:params:xml:ns:caldav}read-free-busy"); err != nil {
		return err
	}

	node, err := p.server.Tree.NodeForPath(r.Context(), path)
	if err != nil {
		return err
	}
	calNode, isCal := node.(*Calendar)
	if !isCal {
		return dav.ErrNotImplemented("The free-busy-query REPORT is only implemented on calendars")
	}

	// A transparent (private) calendar contributes no busy time (RFC 6638
	// §9.1), so it answers with an empty VFREEBUSY rather than its events'
	// busy periods — keeping a private calendar's schedule invisible even to a
	// direct free-busy-query.
	var payloads [][]byte
	if !calNode.transparent() {
		objects, err := calNode.QueryObjects(r.Context(), "", start, end)
		if err != nil {
			return err
		}
		for _, obj := range objects {
			payloads = append(payloads, obj.Data())
		}
	}
	result, err := ical.GenerateFreeBusy(payloads, start, end, p.calendarTimeZone(r.Context(), path), time.Now())
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/calendar")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(result)
	return err
}

// calendarTimeZone resolves a calendar's timezone from its
// calendar-timezone property; UTC when unset, like sabre.
func (p *Plugin) calendarTimeZone(ctx context.Context, calendarPath string) *time.Location {
	node, err := p.server.Tree.NodeForPath(ctx, calendarPath)
	if err != nil {
		return time.UTC
	}
	calNode, isCal := node.(*Calendar)
	if !isCal {
		return time.UTC
	}
	tzProp, exists := calNode.Info.Props[xml.Name{Space: davxml.NSCalDAV, Local: "calendar-timezone"}]
	if !exists {
		return time.UTC
	}
	tzData, isString := tzProp.(string)
	if !isString {
		return time.UTC
	}
	vcal, err := ical.Decode([]byte(tzData))
	if err != nil {
		return time.UTC
	}
	for _, child := range vcal.Children {
		if child.Name == "VTIMEZONE" {
			if tzid := child.Props.Get("TZID"); tzid != nil {
				if loc, err := time.LoadLocation(tzid.Value); err == nil {
					return loc
				}
			}
		}
	}
	return time.UTC
}

func (p *Plugin) calendarQueryReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	query, err := ParseCalendarQuery(body)
	if err != nil {
		var davErr *dav.Error
		if errors.As(err, &davErr) {
			return err
		}
		return dav.ErrBadRequest("%s", err)
	}
	node, err := p.server.Tree.NodeForPath(r.Context(), path)
	if err != nil {
		return err
	}
	depth := 0
	if d := r.Header.Get("Depth"); d == "1" || strings.EqualFold(d, "infinity") {
		depth = 1
	}
	// Microsoft clients send Depth: 0 but mean 1, like sabre notes.
	if depth == 0 && strings.HasPrefix(r.Header.Get("User-Agent"), "MSFT-") {
		if _, isCal := node.(*Calendar); isCal {
			depth = 1
		}
	}

	loc := time.UTC
	if query.Expand != nil {
		calendarPath := path
		if _, isObj := node.(*CalendarObject); isObj {
			calendarPath, _ = pathutil.Split(path)
		}
		loc = p.calendarTimeZone(r.Context(), calendarPath)
	}
	validator := &Validator{Loc: loc}

	// The matched objects are already loaded, so their responses are built
	// from these nodes rather than by resolving each path again.
	var refs []objectRef
	switch n := node.(type) {
	case *CalendarObject:
		if depth == 0 {
			ok, err := p.objectMatches(n, query, validator)
			if err != nil {
				return err
			}
			if ok {
				refs = append(refs, objectRef{path: path, node: n})
			}
		}
	case *Calendar:
		if depth == 1 {
			// Narrow the candidate set in the backend (component type +
			// occurrence bounds) instead of loading every object; the
			// validator below stays authoritative.
			componentType, trStart, trEnd := query.prefilterHints()
			objs, err := n.QueryObjects(r.Context(), componentType, trStart, trEnd)
			if err != nil {
				return err
			}
			for _, obj := range objs {
				ok, err := p.objectMatches(obj, query, validator)
				if err != nil {
					// One unparseable/semantically-invalid stored object
					// (e.g. a malformed DURATION written by another client)
					// must not 500 the entire collection query: skip it and
					// keep matching the rest, like a resilient report engine.
					slog.Warn("skipping calendar object in query",
						"path", path+"/"+obj.Name(), "err", err)
					continue
				}
				if ok {
					refs = append(refs, objectRef{path: path + "/" + obj.Name(), node: obj})
				}
			}
		}
	}

	return p.writeObjectResponses(r.Context(), w, refs, query.Props, query.Expand, query.JSON,
		func(string) *time.Location { return loc })
}

func (p *Plugin) objectMatches(obj *CalendarObject, query *CalendarQuery, v *Validator) (bool, error) {
	cal, err := ical.Decode(obj.Data())
	if err != nil {
		return false, dav.ErrBadRequest("The calendar object could not be parsed: %s", err)
	}
	ok, err := v.Validate(cal, query.Filter)
	if err != nil {
		// Semantically-invalid stored data (bad DURATION/date/RRULE in a
		// VALARM/VTODO the PUT-time check doesn't cover) is a client-visible
		// data error, not a server fault: 400 rather than 500.
		return false, invalidCalendarData("Invalid calendar object: %s", err)
	}
	return ok, nil
}

func (p *Plugin) calendarMultigetReport(body []byte, w http.ResponseWriter, r *http.Request) error {
	query, err := ParseCalendarMultiget(body)
	if err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	var refs []objectRef
	for _, href := range query.Hrefs {
		// Normalize each href through the same base-stripping logic as the
		// request path (gopath.Clean + base-prefix enforcement) rather than a
		// bare TrimPrefix, so hrefs outside the mount are rejected instead of
		// silently resolved. A rejected href is answered 404, like a missing
		// object, rather than dropped from the response.
		path, err := p.server.ResolveHref(href)
		if err != nil {
			refs = append(refs, objectRef{href: href})
			continue
		}
		refs = append(refs, objectRef{path: path})
	}
	p.resolveObjectRefs(r.Context(), refs)
	// The hrefs may span calendars, so each is expanded in its own
	// calendar's timezone.
	zones := map[string]*time.Location{}
	locFor := func(objPath string) *time.Location {
		calendarPath, _ := pathutil.Split(objPath)
		loc, seen := zones[calendarPath]
		if !seen {
			loc = p.calendarTimeZone(r.Context(), calendarPath)
			zones[calendarPath] = loc
		}
		return loc
	}
	return p.writeObjectResponses(r.Context(), w, refs, query.Props, query.Expand, query.JSON, locFor)
}

// objectRef is one entry of a report's answer: an object's tree path, or for
// a multiget href that names no path on this server, only the href as sent.
// A report that already holds the object's node sets it, and the answer is
// built from that node instead of resolving the path again.
type objectRef struct {
	path string
	href string
	node dav.Node
}

// writeObjectResponses builds the multistatus for matched objects, applying
// calendar-data expansion (in the timezone locFor gives for the object's
// path) when requested.
func (p *Plugin) writeObjectResponses(ctx context.Context, w http.ResponseWriter, refs []objectRef, props []xml.Name, expand *TimeRange, asJSON bool, locFor func(string) *time.Location) error {
	var paths []string
	for _, ref := range refs {
		if ref.path != "" {
			paths = append(paths, ref.path)
		}
	}
	if len(paths) > 1 {
		if err := p.server.PrefetchPaths(ctx, paths); err != nil {
			return err
		}
	}
	ms := &davxml.MultiStatus{}
	for _, ref := range refs {
		if ref.path == "" {
			ms.Responses = append(ms.Responses, davxml.Response{Href: ref.href, Status: http.StatusNotFound})
			continue
		}
		objPath := ref.path
		var responses []davxml.Response
		var err error
		if ref.node != nil {
			var resp davxml.Response
			resp, err = p.server.PropertiesForNode(ctx, objPath, ref.node, props)
			responses = []davxml.Response{resp}
		} else {
			responses, err = p.server.PropertiesForPath(ctx, objPath, props, 0)
		}
		if err != nil {
			var davErr *dav.Error
			if errors.As(err, &davErr) && davErr.Status == http.StatusNotFound {
				ms.Responses = append(ms.Responses, davxml.Response{
					Href:   p.server.Href(objPath),
					Status: http.StatusNotFound,
				})
				continue
			}
			return err
		}
		for _, resp := range responses {
			if expand != nil {
				if err := expandResponseData(&resp, expand, locFor(objPath)); err != nil {
					return err
				}
			}
			if asJSON {
				if err := jsonResponseData(&resp); err != nil {
					return err
				}
			}
			ms.Responses = append(ms.Responses, resp)
		}
	}

	w.Header().Set("Vary", "Brief,Prefer")
	return p.server.WriteMultiStatus(w, ms)
}

// resolveObjectRefs fills in the nodes of refs that name objects of the same
// calendar: the calendar is resolved once per distinct parent path and its
// objects are fetched in one batch, instead of a tree walk per href. A
// parent that is not a calendar, or fails to resolve, leaves its refs for
// the per-path lookup, which answers them (404 included) as before.
func (p *Plugin) resolveObjectRefs(ctx context.Context, refs []objectRef) {
	byParent := map[string][]int{}
	var parents []string
	for i, ref := range refs {
		if ref.path == "" {
			continue
		}
		parent, _ := pathutil.Split(ref.path)
		if _, seen := byParent[parent]; !seen {
			parents = append(parents, parent)
		}
		byParent[parent] = append(byParent[parent], i)
	}
	for _, parent := range parents {
		node, err := p.server.Tree.NodeForPath(ctx, parent)
		if err != nil {
			continue
		}
		cal, isCal := node.(*Calendar)
		if !isCal {
			continue
		}
		uris := make([]string, 0, len(byParent[parent]))
		for _, i := range byParent[parent] {
			_, uri := pathutil.Split(refs[i].path)
			uris = append(uris, uri)
		}
		objs, err := cal.objectsByURI(ctx, uris)
		if err != nil {
			continue
		}
		for _, i := range byParent[parent] {
			_, uri := pathutil.Split(refs[i].path)
			if obj := objs[uri]; obj != nil {
				refs[i].node = obj
			} else {
				// Resolved the calendar and the object is not there: the
				// per-path lookup would say 404 too, without the walk.
				refs[i].node = nil
				refs[i].path, refs[i].href = "", p.server.Href(refs[i].path)
			}
		}
	}
}

// jsonResponseData rewrites the calendar-data property of a response as
// jCal, for content-type="application/calendar+json" requests.
func jsonResponseData(resp *davxml.Response) error {
	for pi := range resp.Propstats {
		ps := &resp.Propstats[pi]
		if ps.Status != http.StatusOK {
			continue
		}
		for i, prop := range ps.Props {
			if prop.Name != nameCalendarData {
				continue
			}
			text, isText := prop.Value.(davxml.Text)
			if !isText {
				continue
			}
			cal, err := ical.Decode([]byte(ical.NormalizeCRLF(string(text))))
			if err != nil {
				return dav.ErrBadRequest("The calendar object could not be parsed: %s", err)
			}
			out, err := ical.JCal(cal)
			if err != nil {
				return err
			}
			ps.Props[i].Value = davxml.Text(out)
		}
	}
	return nil
}

// HandleGet implements dav.GetHandlerHook: calendar objects negotiate to
// jCal when the client asks for application/calendar+json (sabre's
// httpAfterGet transform).
func (p *Plugin) HandleGet(node dav.Node, w http.ResponseWriter, r *http.Request) (bool, error) {
	obj, isObject := node.(*CalendarObject)
	if !isObject || negotiateExportFormat(r.Header.Get("Accept")) != "application/calendar+json" {
		return false, nil
	}
	cal, err := ical.Decode(obj.Data())
	if err != nil {
		return true, dav.ErrBadRequest("The calendar object could not be parsed: %s", err)
	}
	body, err := ical.JCal(cal)
	if err != nil {
		return true, err
	}
	h := w.Header()
	h.Set("Content-Type", "application/calendar+json")
	if etag := obj.ETag(); etag != "" {
		h.Set("ETag", etag)
	}
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	return true, err
}

// expandResponseData rewrites the calendar-data property of a response with
// its recurrence-expanded form.
func expandResponseData(resp *davxml.Response, expand *TimeRange, loc *time.Location) error {
	for pi := range resp.Propstats {
		ps := &resp.Propstats[pi]
		if ps.Status != http.StatusOK {
			continue
		}
		for i, prop := range ps.Props {
			if prop.Name != nameCalendarData {
				continue
			}
			text, isText := prop.Value.(davxml.Text)
			if !isText {
				continue
			}
			// The propfind handler strips \r; restore CRLF for parsing.
			cal, err := ical.Decode([]byte(ical.NormalizeCRLF(string(text))))
			if err != nil {
				return dav.ErrBadRequest("The calendar object could not be parsed: %s", err)
			}
			expanded, err := ical.Expand(cal, expand.Start, expand.End, loc)
			if err != nil {
				// Malformed recurrence/duration data in a stored object is a
				// data problem, not a server fault: report 400
				// valid-calendar-data instead of a generic 500.
				return invalidCalendarData("Could not expand calendar object: %s", err)
			}
			ps.Props[i].Value = davxml.Text(ical.EncodeLenient(expanded))
		}
	}
	return nil
}
