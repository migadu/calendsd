// The schedule outbox POST and free-busy aggregation (RFC 6638 §5).

package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"

	"github.com/migadu/calendsd/internal/caldav/ical"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/pathutil"
)

// HandleMethod implements the outbox POST (RFC 6638 §5).
func (p *SchedulePlugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != "POST" {
		return false, nil
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/calendar") {
		return false, nil
	}
	path, err := p.server.Path(r)
	if err != nil {
		return false, nil
	}
	node, err := p.server.Tree.NodeForPath(r.Context(), path)
	if err != nil {
		return false, nil
	}
	if _, isOutbox := node.(*Outbox); !isOutbox {
		return false, nil
	}
	// POST is not covered by the ACL beforeMethod table, so enforce
	// schedule-send here — otherwise any authenticated user could probe
	// free-busy from any principal's outbox.
	checker, err := RequirePrivilegeChecker(p.server)
	if err != nil {
		return true, err
	}
	if err := checker.CheckPrivileges(r.Context(), path,
		"{urn:ietf:params:xml:ns:caldav}schedule-send"); err != nil {
		return true, err
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return true, err
	}
	vcal, err := ical.Decode(body)
	if err != nil {
		return true, dav.ErrBadRequest("The request body must be a valid iCalendar object")
	}

	var componentType string
	for _, comp := range vcal.Children {
		if comp.Name != goical.CompTimezone {
			componentType = comp.Name
			break
		}
	}
	if componentType == "" {
		return true, dav.ErrBadRequest("We expected at least one VTODO, VJOURNAL, VFREEBUSY or VEVENT component")
	}
	methodProp := vcal.Props.Get("METHOD")
	if methodProp == nil {
		return true, dav.ErrBadRequest("A METHOD property must be specified in iTIP messages")
	}

	if componentType == "VFREEBUSY" && methodProp.Value == "REQUEST" {
		return true, p.freeBusyOutboxRequest(w, r, vcal)
	}
	return true, dav.ErrNotImplemented("SabreDAV supports only VFREEBUSY (REQUEST) on this endpoint")
}

// freeBusyOutboxRequest answers a free-busy request for each attendee with
// their aggregated availability. Subset of sabre's handleFreeBusyRequest.
func (p *SchedulePlugin) freeBusyOutboxRequest(w http.ResponseWriter, r *http.Request, vcal *goical.Calendar) error {
	var vfb *goical.Component
	for _, comp := range vcal.Children {
		if comp.Name == "VFREEBUSY" {
			vfb = comp
			break
		}
	}
	if vfb == nil {
		return dav.ErrBadRequest("The freebusy request must contain a VFREEBUSY component")
	}
	startProp, endProp := vfb.Props.Get("DTSTART"), vfb.Props.Get("DTEND")
	if startProp == nil || endProp == nil {
		return dav.ErrBadRequest("DTSTART and DTEND must be specified")
	}
	start, err := startProp.DateTime(nil)
	if err != nil {
		return dav.ErrBadRequest("Invalid DTSTART")
	}
	end, err := endProp.DateTime(nil)
	if err != nil {
		return dav.ErrBadRequest("Invalid DTEND")
	}
	// A window wider than the server will expand answers every recipient
	// with a terminal 5.3 rather than a truncated schedule (audit 3.1, D1).
	// The envelope is still a 200 schedule-response, like any per-recipient
	// failure, so a client sees which recipients it must re-ask.
	var spanStatus string
	if _, _, err := freeBusyWindow(start, end); err != nil {
		var de *dav.Error
		if errors.As(err, &de) {
			spanStatus = "5.3;" + de.Message
		} else {
			spanStatus = "5.3;" + err.Error()
		}
	}

	// Identity properties echoed into each recipient's VFREEBUSY, matching
	// sabre's generateFreeBusyResponse (UID + ORGANIZER from the request,
	// ATTENDEE set to the recipient).
	var idProps []string
	if uid := vfb.Props.Get("UID"); uid != nil && uid.Value != "" {
		idProps = appendPropLine(idProps, uid)
	}
	if org := vfb.Props.Get("ORGANIZER"); org != nil {
		idProps = appendPropLine(idProps, org)
	}

	var resp scheduleResponseXML
	// The per-recipient aggregation below skips calendars the requester
	// holds no read-free-busy on, so it needs the same checker.
	checker, err := RequirePrivilegeChecker(p.server)
	if err != nil {
		return err
	}
	for _, att := range vfb.Props.Values("ATTENDEE") {
		status := "2.0;Success"
		var fbData string
		principal := principalForAddress(r.Context(), p.server, att.Value)
		if spanStatus != "" {
			status = spanStatus
		} else if principal == "" {
			status = "3.7;Could not find principal"
		} else {
			extra := appendPropLine(append([]string{}, idProps...), &goical.Prop{Name: "ATTENDEE", Value: att.Value})
			fb, readable, err := p.freeBusyForPrincipal(r.Context(), checker, principal, start.UTC(), end.UTC(), extra...)
			if err != nil {
				status = "5.0;Internal error"
			} else if !readable {
				// No calendar of the recipient was readable (e.g. a
				// cross-domain requester). Deliberately the same status as
				// an unknown address, so a denied probe cannot be used to
				// enumerate which users exist.
				status = "3.7;Could not find principal"
			} else {
				fbData = string(fb)
			}
		}
		resp.Responses = append(resp.Responses, scheduleResponseItemXML{
			Recipient: hrefXML{Href: att.Value}, RequestStatus: status, CalendarData: fbData,
		})
	}
	body, err := xml.Marshal(resp)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(append([]byte(xml.Header), body...))
	return err
}

// scheduleResponseXML is the RFC 6638 §5 answer to an outbox POST.
type scheduleResponseXML struct {
	XMLName   xml.Name                  `xml:"urn:ietf:params:xml:ns:caldav schedule-response"`
	Responses []scheduleResponseItemXML `xml:"urn:ietf:params:xml:ns:caldav response"`
}

type scheduleResponseItemXML struct {
	Recipient     hrefXML `xml:"urn:ietf:params:xml:ns:caldav recipient"`
	RequestStatus string  `xml:"urn:ietf:params:xml:ns:caldav request-status"`
	CalendarData  string  `xml:"urn:ietf:params:xml:ns:caldav calendar-data,omitempty"`
}

// freeBusyForPrincipal aggregates free-busy across the principal's calendars
// the requesting user holds read-free-busy on (like sabre's
// getFreeBusyForEmail, which skips calendars that fail that check). The
// returned readable flag is false when no calendar was readable at all, so
// the caller can distinguish "denied" from "free".
func (p *SchedulePlugin) freeBusyForPrincipal(ctx context.Context, checker PrivilegeChecker, principalURI string, start, end time.Time, extraProps ...string) (fb []byte, readable bool, err error) {
	_, name := pathutil.Split(principalURI)
	home, err := p.server.Tree.NodeForPath(ctx, "calendars/"+name)
	if err != nil {
		return nil, false, err
	}
	coll, ok := home.(dav.Collection)
	if !ok {
		return nil, false, nil
	}
	children, err := coll.Children(ctx)
	if err != nil {
		return nil, false, err
	}
	var payloads [][]byte
	for _, child := range children {
		calNode, isCal := child.(*Calendar)
		if !isCal {
			continue
		}
		if checker.CheckPrivileges(ctx, "calendars/"+name+"/"+calNode.Name(),
			"{urn:ietf:params:xml:ns:caldav}read-free-busy") != nil {
			continue
		}
		// The requester may read this calendar's free-busy, so the principal is
		// schedulable — a valid (possibly empty) response follows even if every
		// readable calendar turns out to be private. This must be set before the
		// transparent skip so an all-private principal reads as "free", not as
		// the "could not find principal" status used for a genuine denial.
		readable = true
		// A transparent (private) calendar contributes no busy time (RFC 6638
		// §9.1): it stays hidden but does not turn the response into "not found".
		if calNode.transparent() {
			continue
		}
		objs, err := calNode.QueryObjects(ctx, "", start, end)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			payloads = append(payloads, obj.Data())
		}
	}
	if !readable {
		return nil, false, nil
	}
	// Working hours apply only now that the requester has passed the
	// read-free-busy check, so they reach exactly who could already see busy
	// time. A principal whose calendars are all private still reports them:
	// hours are not calendar content (docs/design/VAVAILABILITY-PLAN.md §4.3).
	if availability := p.availabilityFor(ctx, name); availability != nil {
		fb, err = ical.GenerateFreeBusyWithAvailability(payloads, availability, start, end, time.UTC, time.Now(), extraProps...)
		if !errors.Is(err, ical.ErrInvalidAvailability) {
			return fb, true, err
		}
		// A value validation never saw (one imported from a dead property)
		// must not turn the probe into an error: answer without hours.
		slog.Warn("ignoring unusable calendar-availability", "principal", principalURI, "err", err)
	}
	fb, err = ical.GenerateFreeBusy(payloads, start, end, time.UTC, time.Now(), extraProps...)
	return fb, true, err
}

// availabilityFor returns the stored calendar-availability of the user whose
// home is calendars/<name>, or nil when none is set or it cannot be read.
func (p *SchedulePlugin) availabilityFor(ctx context.Context, name string) []byte {
	node, err := p.server.Tree.NodeForPath(ctx, "calendars/"+name+"/inbox")
	if err != nil {
		return nil
	}
	inbox, ok := node.(*Inbox)
	if !ok {
		return nil
	}
	ab := inbox.availability()
	if ab == nil {
		return nil
	}
	data, err := ab.Availability(ctx, inbox.principalURI)
	if err != nil {
		slog.Warn("reading calendar-availability", "principal", inbox.principalURI, "err", err)
		return nil
	}
	return data
}

// appendPropLine appends prop as a content line, serialized by go-ical so
// parameter values with ";:," are quoted (an ORGANIZER CN like "Doe, Jane"
// stays one parameter) and long lines are folded. A property go-ical refuses
// to encode (a parameter value with a DQUOTE) is left out.
func appendPropLine(lines []string, prop *goical.Prop) []string {
	comp := goical.NewComponent("X")
	comp.Props.Add(&goical.Prop{Name: prop.Name, Params: prop.Params, Value: prop.Value})
	var buf bytes.Buffer
	if err := goical.NewEncoder(&buf).Encode(&goical.Calendar{Component: comp}); err != nil {
		return lines
	}
	line := strings.TrimPrefix(buf.String(), "BEGIN:X\r\n")
	line = strings.TrimSuffix(line, "\r\nEND:X\r\n")
	return append(lines, line)
}
