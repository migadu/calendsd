package caldav

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"

	"calserver/internal/acl"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// SchedulePlugin implements CalDAV scheduling (RFC 6638): scheduling
// properties, iTIP delivery on calendar-object writes and deletes, and the
// schedule outbox POST. Port of the local-delivery core of sabre's
// Schedule\Plugin (iMip email delivery is out of scope).
type SchedulePlugin struct {
	server *dav.Server
}

func (p *SchedulePlugin) PluginName() string { return "caldav-schedule" }

func (p *SchedulePlugin) Init(s *dav.Server) { p.server = s }

func (p *SchedulePlugin) Features() []string { return []string{"calendar-auto-schedule"} }

func (p *SchedulePlugin) Methods(path string) []string { return []string{"POST"} }

// PropFind provides the scheduling properties on principals.
func (p *SchedulePlugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	cal := func(local string) xml.Name { return xml.Name{Space: davxml.NSCalDAV, Local: local} }

	ph, isPrincipal := node.(acl.PrincipalHolder)
	if !isPrincipal {
		return nil
	}
	_, principalName := splitPath(ph.PrincipalURL())
	homePath := p.server.BaseURI + "calendars/" + principalName + "/"

	pf.Handle(cal("schedule-inbox-URL"), func() any {
		return davxml.Href{homePath + "inbox/"}
	})
	pf.Handle(cal("schedule-outbox-URL"), func() any {
		return davxml.Href{homePath + "outbox/"}
	})
	pf.Handle(cal("calendar-user-address-set"), func() any {
		hrefs := davxml.Href(ph.AlternateURISet())
		hrefs = append(hrefs, p.server.BaseURI+ph.PrincipalURL()+"/")
		return hrefs
	})
	pf.Handle(cal("schedule-default-calendar-URL"), func() any {
		if path := p.defaultCalendarPath(ph.PrincipalURL()); path != "" {
			return davxml.Href{p.server.BaseURI + path + "/"}
		}
		return nil
	})
	return nil
}

// defaultCalendarPath picks the principal's first VEVENT-capable calendar,
// like sabre's default-calendar search.
func (p *SchedulePlugin) defaultCalendarPath(principalURI string) string {
	_, name := splitPath(principalURI)
	home, err := p.server.Tree.NodeForPath("calendars/" + name)
	if err != nil {
		return ""
	}
	coll, ok := home.(dav.Collection)
	if !ok {
		return ""
	}
	children, err := coll.Children()
	if err != nil {
		return ""
	}
	for _, child := range children {
		if calNode, isCal := child.(*Calendar); isCal {
			return "calendars/" + name + "/" + calNode.Name()
		}
	}
	return ""
}

// addressesForPrincipal returns a principal's calendar user addresses.
func (p *SchedulePlugin) addressesForPrincipal(principalURI string) []string {
	node, err := p.server.Tree.NodeForPath(principalURI)
	if err != nil {
		return nil
	}
	ph, ok := node.(acl.PrincipalHolder)
	if !ok {
		return nil
	}
	return append(ph.AlternateURISet(), p.server.BaseURI+ph.PrincipalURL()+"/")
}

// principalForAddress finds the principal owning a calendar user address
// (sabre's getPrincipalByUri via calendar-user-address-set).
func (p *SchedulePlugin) principalForAddress(address string) string {
	principals, err := p.server.Tree.NodeForPath("principals")
	if err != nil {
		return ""
	}
	coll, ok := principals.(dav.Collection)
	if !ok {
		return ""
	}
	children, err := coll.Children()
	if err != nil {
		return ""
	}
	for _, child := range children {
		ph, isPrincipal := child.(acl.PrincipalHolder)
		if !isPrincipal {
			continue
		}
		for _, candidate := range ph.AlternateURISet() {
			if strings.EqualFold(candidate, address) {
				return ph.PrincipalURL()
			}
		}
	}
	return ""
}

// InterceptWrite implements dav.WriteInterceptor: PUTs of calendar objects
// run through the iTIP broker; SCHEDULE-STATUS updates are written back into
// the stored object (sabre's calendarObjectChange + $modified).
func (p *SchedulePlugin) InterceptWrite(r *http.Request, path string, data []byte, isNew bool) ([]byte, error) {
	if strings.EqualFold(r.Header.Get("Schedule-Reply"), "F") {
		return data, nil
	}
	parentPath, _ := splitPath(path)
	parent, err := p.server.Tree.NodeForPath(parentPath)
	if err != nil {
		return data, nil // not our concern; PUT will surface the error
	}
	calNode, isCal := parent.(*Calendar)
	if !isCal {
		return data, nil
	}

	newCal, err := ical.Decode(data)
	if err != nil {
		return data, nil // CalDAV validation reports parse errors
	}
	var oldCal *goical.Calendar
	if !isNew {
		if node, err := p.server.Tree.NodeForPath(path); err == nil {
			if obj, isObj := node.(*CalendarObject); isObj {
				oldCal, _ = ical.Decode(obj.Data())
			}
		}
	}

	addresses := p.addressesForPrincipal(calNode.Owner())
	messages, err := ical.ParseEvent(newCal, addresses, oldCal)
	if err != nil || len(messages) == 0 {
		return data, nil
	}
	for _, msg := range messages {
		p.deliver(r, msg)
	}
	// Write SCHEDULE-STATUS back onto the stored object's attendees.
	applyScheduleStatus(newCal, messages)
	return ical.EncodeLenient(newCal), nil
}

// BeforeUnbind implements dav.BeforeUnbindHook: deleting a scheduled object
// sends CANCELs (organizer) or a declining REPLY (attendee).
func (p *SchedulePlugin) BeforeUnbind(r *http.Request, path string) error {
	if r.Method == "MOVE" || strings.EqualFold(r.Header.Get("Schedule-Reply"), "F") {
		return nil
	}
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return nil
	}
	obj, isObj := node.(*CalendarObject)
	if !isObj {
		return nil
	}
	oldCal, err := ical.Decode(obj.Data())
	if err != nil {
		return nil
	}
	addresses := p.addressesForPrincipal(obj.Owner())
	messages, err := ical.ParseEvent(nil, addresses, oldCal)
	if err != nil {
		return nil
	}
	for _, msg := range messages {
		p.deliver(r, msg)
	}
	return nil
}

// deliver routes one iTIP message; only local delivery is implemented
// (sabre's schedule event with the local-delivery listener).
func (p *SchedulePlugin) deliver(r *http.Request, msg *ical.Message) {
	p.scheduleLocalDelivery(r, msg)
	if msg.ScheduleStatus == "" {
		msg.ScheduleStatus = "5.2;There was no system capable of delivering the scheduling message"
	}
	// Insignificant messages that couldn't be delivered don't taint the
	// object, like sabre's deliver().
	code, _, _ := strings.Cut(msg.ScheduleStatus, ".")
	if !msg.SignificantChange && (code == "3" || code == "5") {
		msg.ScheduleStatus = ""
	}
}

// scheduleLocalDelivery drops the message into the recipient's inbox and
// updates their calendar copy. Port of sabre's scheduleLocalDelivery.
func (p *SchedulePlugin) scheduleLocalDelivery(r *http.Request, msg *ical.Message) {
	principalURI := p.principalForAddress(msg.Recipient)
	if principalURI == "" {
		msg.ScheduleStatus = "3.7;Could not find principal."
		return
	}
	_, name := splitPath(principalURI)
	homePath := "calendars/" + name

	inboxNode, err := p.server.Tree.NodeForPath(homePath + "/inbox")
	if err != nil {
		msg.ScheduleStatus = "5.2;Could not find local inbox"
		return
	}
	inbox, isInbox := inboxNode.(*Inbox)
	if !isInbox {
		msg.ScheduleStatus = "5.2;Could not find local inbox"
		return
	}
	defaultCalendar := p.defaultCalendarPath(principalURI)
	if defaultCalendar == "" {
		msg.ScheduleStatus = "5.2;Could not find a schedule-default-calendar-URL property"
		return
	}

	// Locate an existing copy by UID across the recipient's calendars.
	var existingObj *CalendarObject
	var existingPath string
	home, _ := p.server.Tree.NodeForPath(homePath)
	if coll, ok := home.(dav.Collection); ok {
		children, _ := coll.Children()
	searchLoop:
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
				if d, err := ical.Denormalize(obj.CalendarData); err == nil && d.UID == msg.UID {
					o := NewCalendarObject(calNode.backend, calNode.Info, obj)
					existingObj = o
					existingPath = homePath + "/" + calNode.Name() + "/" + obj.URI
					break searchLoop
				}
			}
		}
	}

	var existingCal *goical.Calendar
	if existingObj != nil {
		existingCal, _ = ical.Decode(existingObj.Data())
	}
	newObject, err := ical.ProcessMessage(msg, existingCal)
	if err != nil || (newObject == nil && msg.Method != "CANCEL") {
		msg.ScheduleStatus = "5.0;iTip message was not processed by the server, likely because we didn't understand it."
		return
	}

	// The raw iTIP message lands in the inbox.
	fileName := "calserver-" + randomToken() + ".ics"
	if _, err := inbox.CreateFile(fileName, strings.NewReader(string(ical.EncodeLenient(msg.Calendar)))); err != nil {
		msg.ScheduleStatus = "5.2;Could not deliver to local inbox"
		return
	}

	if newObject != nil {
		serialized := ical.EncodeLenient(newObject)
		if existingObj != nil {
			if _, err := existingObj.Put(strings.NewReader(string(serialized))); err != nil {
				msg.ScheduleStatus = "5.2;Could not update the recipient calendar"
				return
			}
			_ = existingPath
		} else {
			calendarNode, err := p.server.Tree.NodeForPath(defaultCalendar)
			if err != nil {
				msg.ScheduleStatus = "5.2;Could not find the default calendar"
				return
			}
			if coll, ok := calendarNode.(dav.Collection); ok {
				if _, err := coll.CreateFile(fileName, strings.NewReader(string(serialized))); err != nil {
					msg.ScheduleStatus = "5.2;Could not deliver to the default calendar"
					return
				}
			}
		}
	}
	msg.ScheduleStatus = "1.2;Message delivered locally"
}

// applyScheduleStatus writes each message's delivery status onto the
// matching ATTENDEE (or ORGANIZER for replies) of the stored object.
func applyScheduleStatus(cal *goical.Calendar, messages []*ical.Message) {
	for _, msg := range messages {
		if msg.ScheduleStatus == "" {
			continue
		}
		code := msg.ScheduleStatusCode()
		target := "ATTENDEE"
		if msg.Method == "REPLY" {
			target = "ORGANIZER"
		}
		for _, comp := range cal.Children {
			if comp.Name != goical.CompEvent {
				continue
			}
			props := comp.Props.Values(target)
			for i := range props {
				if strings.EqualFold(props[i].Value, msg.Recipient) {
					props[i].Params.Set("SCHEDULE-STATUS", code)
				}
			}
			if len(props) > 0 {
				comp.Props[target] = props
			}
		}
	}
}

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
			fb, err := p.freeBusyForPrincipal(principal, start.UTC(), end.UTC())
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
func (p *SchedulePlugin) freeBusyForPrincipal(principalURI string, start, end time.Time) ([]byte, error) {
	_, name := splitPath(principalURI)
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
	return ical.GenerateFreeBusy(payloads, start, end, time.UTC, time.Now())
}

func randomToken() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func xmlEscapeText(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
