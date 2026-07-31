// The schedule outbox POST and free-busy aggregation (RFC 6638 §5).

package caldav

import (
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"

	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	"calserver/internal/pathutil"
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
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return false, nil
	}
	if _, isOutbox := node.(*Outbox); !isOutbox {
		return false, nil
	}
	// POST is not covered by the ACL beforeMethod table, so enforce
	// schedule-send here — otherwise any authenticated user could probe
	// free-busy from any principal's outbox.
	for _, plugin := range p.server.Plugins() {
		if checker, ok := plugin.(PrivilegeChecker); ok {
			if err := checker.CheckPrivileges(r.Context(), path,
				"{urn:ietf:params:xml:ns:caldav}schedule-send"); err != nil {
				return true, err
			}
		}
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

	// Identity properties echoed into each recipient's VFREEBUSY, matching
	// sabre's generateFreeBusyResponse (UID + ORGANIZER from the request,
	// ATTENDEE set to the recipient).
	var idProps []string
	if uid := vfb.Props.Get("UID"); uid != nil && uid.Value != "" {
		idProps = append(idProps, "UID:"+uid.Value)
	}
	if org := vfb.Props.Get("ORGANIZER"); org != nil {
		idProps = append(idProps, formatICalProp(org))
	}

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<cal:schedule-response xmlns:d="DAV:" xmlns:cal="urn:ietf:params:xml:ns:caldav">` + "\n")
	for _, att := range vfb.Props.Values("ATTENDEE") {
		status := "2.0;Success"
		var fbData string
		principal := p.principalForAddress(att.Value)
		if principal == "" {
			status = "3.7;Could not find principal"
		} else {
			extra := append(append([]string{}, idProps...), "ATTENDEE:"+att.Value)
			fb, err := p.freeBusyForPrincipal(principal, start.UTC(), end.UTC(), extra...)
			if err != nil {
				status = "5.0;Internal error"
			} else {
				fbData = string(fb)
			}
		}
		b.WriteString(" <cal:response>\n")
		b.WriteString("  <cal:recipient><d:href>" + xmlEscapeText(att.Value) + "</d:href></cal:recipient>\n")
		b.WriteString("  <cal:request-status>" + xmlEscapeText(status) + "</cal:request-status>\n")
		if fbData != "" {
			b.WriteString("  <cal:calendar-data>" + xmlEscapeText(fbData) + "</cal:calendar-data>\n")
		}
		b.WriteString(" </cal:response>\n")
	}
	b.WriteString("</cal:schedule-response>\n")

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, err = io.WriteString(w, b.String())
	return err
}

// freeBusyForPrincipal aggregates free-busy across all of a principal's
// calendars.
func (p *SchedulePlugin) freeBusyForPrincipal(principalURI string, start, end time.Time, extraProps ...string) ([]byte, error) {
	_, name := pathutil.Split(principalURI)
	home, err := p.server.Tree.NodeForPath("calendars/" + name)
	if err != nil {
		return nil, err
	}
	coll, ok := home.(dav.Collection)
	if !ok {
		return nil, nil
	}
	children, err := coll.Children()
	if err != nil {
		return nil, err
	}
	var payloads [][]byte
	for _, child := range children {
		calNode, isCal := child.(*Calendar)
		if !isCal {
			continue
		}
		objs, err := calNode.backend.CalendarObjects(calNode.Info.ID)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			payloads = append(payloads, obj.CalendarData)
		}
	}
	return ical.GenerateFreeBusy(payloads, start, end, time.UTC, time.Now(), extraProps...)
}

// formatICalProp serializes an iCalendar property to a single unfolded content
// line ("NAME;PARAM=value:value"), preserving parameters so a cloned ORGANIZER
// keeps its CN.
func formatICalProp(prop *goical.Prop) string {
	var b strings.Builder
	b.WriteString(prop.Name)
	// Emit parameters in a stable order for deterministic output.
	names := make([]string, 0, len(prop.Params))
	for name := range prop.Params {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range prop.Params[name] {
			b.WriteString(";" + name + "=" + v)
		}
	}
	b.WriteString(":" + prop.Value)
	return b.String()
}

func xmlEscapeText(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
