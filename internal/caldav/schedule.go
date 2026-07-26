package caldav

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
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

	// ExpandGroups turns a group ATTENDEE into a display-only
	// CUTYPE=X-SERVER-GROUP placeholder plus one real ATTENDEE per member
	// (Calendar Server's group-attendee expansion); off by default.
	ExpandGroups bool

	// AutoSchedule marks principals that auto-process scheduling messages
	// (Calendar-Server resource/location semantics): REQUESTs are accepted
	// into their default calendar — or declined on a free-busy conflict —
	// without an inbox drop, and a REPLY goes back to the organizer.
	// Nil disables auto-processing (sabre behavior).
	AutoSchedule func(principalURI string) bool

	// pendingReplies queues auto-generated REPLYs per in-flight request:
	// they are produced inside the organizer's PUT (InterceptWrite), but can
	// only be applied to the organizer's copy after that PUT has been
	// persisted, so AfterWrite flushes them. Keyed by *http.Request.
	pendingReplies sync.Map
}

func (p *SchedulePlugin) PluginName() string { return "caldav-schedule" }

func (p *SchedulePlugin) Init(s *dav.Server) { p.server = s }

func (p *SchedulePlugin) Features() []string {
	f := []string{"calendar-auto-schedule"}
	if p.ExpandGroups {
		f = append(f, "calendarserver-group-attendee")
	}
	return f
}

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

// principalEmail returns a principal node's email address, if any.
func principalEmail(node dav.Node) string {
	pn, ok := node.(dav.PropNode)
	if !ok {
		return ""
	}
	props, err := pn.Props([]xml.Name{{Space: davxml.NSSabreDAV, Local: "email-address"}})
	if err != nil {
		return ""
	}
	if v, ok := props[xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}].(string); ok {
		return v
	}
	return ""
}

// principalName returns a principal node's display name, if any.
func principalName(node dav.Node) string {
	pn, ok := node.(dav.PropNode)
	if !ok {
		return ""
	}
	props, err := pn.Props([]xml.Name{{Space: davxml.NSDav, Local: "displayname"}})
	if err != nil {
		return ""
	}
	if v, ok := props[xml.Name{Space: davxml.NSDav, Local: "displayname"}].(string); ok {
		return v
	}
	return ""
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

	expanded := false
	if p.ExpandGroups {
		expanded = p.expandGroupAttendees(newCal)
	}

	addresses := p.addressesForPrincipal(calNode.Owner())
	messages, err := ical.ParseEvent(newCal, addresses, oldCal)
	if err != nil || len(messages) == 0 {
		// Re-encode only when group expansion rewrote the object; a plain
		// non-scheduling PUT must be stored byte-for-byte (parity).
		if expanded {
			return ical.EncodeLenient(newCal), nil
		}
		return data, nil
	}
	for _, msg := range messages {
		p.deliver(r, msg)
	}
	// Write SCHEDULE-STATUS back onto the stored object's attendees.
	applyScheduleStatus(newCal, messages)
	return ical.EncodeLenient(newCal), nil
}

// groupMemberInfo is one expanded member of a group attendee.
type groupMemberInfo struct{ address, name, email string }

// groupMembers reports whether the calendar-user address is a group
// principal, and if so its members' scheduling info. A principal counts as a
// group when it carries a non-empty {DAV:}group-member-set (only groups and
// proxy sub-principals do, and proxy sub-principals are never attendees).
func (p *SchedulePlugin) groupMembers(address string) (members []groupMemberInfo, groupEmail string, isGroup bool) {
	uri := p.principalForAddress(address)
	if uri == "" {
		return nil, "", false
	}
	node, err := p.server.Tree.NodeForPath(uri)
	if err != nil {
		return nil, "", false
	}
	holder, ok := node.(acl.PrincipalHolder)
	if !ok {
		return nil, "", false
	}
	set, err := holder.GroupMemberSet()
	if err != nil || len(set) == 0 {
		return nil, "", false
	}
	groupEmail = principalEmail(node)
	for _, memberURI := range set {
		mnode, err := p.server.Tree.NodeForPath(memberURI)
		if err != nil {
			continue
		}
		mh, ok := mnode.(acl.PrincipalHolder)
		if !ok {
			continue
		}
		addr := ""
		for _, a := range mh.AlternateURISet() {
			if strings.HasPrefix(strings.ToLower(a), "mailto:") {
				addr = a
				break
			}
		}
		if addr == "" {
			addr = p.server.BaseURI + memberURI + "/"
		}
		members = append(members, groupMemberInfo{
			address: addr, name: principalName(mnode), email: principalEmail(mnode),
		})
	}
	return members, groupEmail, true
}

// expandGroupAttendees rewrites each group ATTENDEE into a display-only
// X-SERVER-GROUP placeholder and appends one real ATTENDEE per member,
// mirroring Calendar Server's GroupAttendeeReconciliation. Returns whether it
// changed the calendar.
func (p *SchedulePlugin) expandGroupAttendees(cal *goical.Calendar) bool {
	changed := false
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		atts := comp.Props["ATTENDEE"]
		// Existing attendee addresses, so a member already invited directly
		// is not added twice.
		present := map[string]bool{}
		for i := range atts {
			present[strings.ToLower(atts[i].Value)] = true
		}
		var added []goical.Prop
		for i := range atts {
			if atts[i].Params.Get("CUTYPE") == "X-SERVER-GROUP" {
				continue // already expanded
			}
			members, groupEmail, isGroup := p.groupMembers(atts[i].Value)
			if !isGroup {
				continue
			}
			changed = true
			// The group attendee becomes a non-schedulable placeholder.
			if atts[i].Params == nil {
				atts[i].Params = make(goical.Params)
			}
			atts[i].Params.Set("CUTYPE", "X-SERVER-GROUP")
			atts[i].Params.Set("SCHEDULE-STATUS", "2.7")
			if groupEmail != "" {
				atts[i].Params.Set("EMAIL", groupEmail)
			}
			atts[i].Params.Del("PARTSTAT")
			atts[i].Params.Del("RSVP")

			for _, m := range members {
				if present[strings.ToLower(m.address)] {
					continue
				}
				present[strings.ToLower(m.address)] = true
				member := goical.NewProp("ATTENDEE")
				member.Value = m.address
				member.Params.Set("CUTYPE", "INDIVIDUAL")
				member.Params.Set("PARTSTAT", "NEEDS-ACTION")
				member.Params.Set("RSVP", "TRUE")
				member.Params.Set("MEMBER", atts[i].Value)
				if m.name != "" {
					member.Params.Set("CN", m.name)
				}
				if m.email != "" {
					member.Params.Set("EMAIL", m.email)
				}
				added = append(added, *member)
			}
		}
		if len(added) > 0 {
			comp.Props["ATTENDEE"] = append(atts, added...)
		}
	}
	return changed
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

// ExternalDeliverer delivers to non-local recipients (iMip email). Returns
// true if it handled the message (set a schedule status).
type ExternalDeliverer interface {
	DeliverExternal(msg *ical.Message) bool
}

// deliver routes one iTIP message: local delivery first, then any external
// deliverers (iMip). Port of sabre's deliver() + the schedule listeners.
func (p *SchedulePlugin) deliver(r *http.Request, msg *ical.Message) {
	if p.principalForAddress(msg.Recipient) != "" {
		p.scheduleLocalDelivery(r, msg)
	} else {
		for _, plugin := range p.server.Plugins() {
			if ext, ok := plugin.(ExternalDeliverer); ok {
				if ext.DeliverExternal(msg) {
					break
				}
			}
		}
	}
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

	if p.AutoSchedule != nil && p.AutoSchedule(principalURI) {
		p.autoProcess(r, msg, principalURI, defaultCalendar, newObject, existingObj)
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

// autoProcess handles a scheduling message for an auto-schedule principal
// (Calendar-Server resource/location semantics): no inbox drop; REQUESTs are
// stored with PARTSTAT=ACCEPTED — or DECLINED when the booking window
// conflicts with existing events — and a REPLY is sent to the organizer;
// CANCELs remove the copy.
func (p *SchedulePlugin) autoProcess(r *http.Request, msg *ical.Message, principalURI, defaultCalendar string, newObject *goical.Calendar, existingObj *CalendarObject) {
	if msg.Method == "CANCEL" {
		if existingObj != nil {
			if err := existingObj.Delete(); err != nil {
				msg.ScheduleStatus = "5.2;Could not update the recipient calendar"
				return
			}
		}
		msg.ScheduleStatus = "1.2;Message delivered locally"
		return
	}

	// Decide participation: decline on a free-busy conflict, else accept.
	preState := ical.EncodeLenient(newObject) // pre-decision copy for the REPLY diff
	partStat := "ACCEPTED"
	if p.hasConflict(principalURI, preState) {
		partStat = "DECLINED"
	}
	setAttendeePartStat(newObject, msg.Recipient, partStat)
	serialized := ical.EncodeLenient(newObject)

	if existingObj != nil {
		if _, err := existingObj.Put(strings.NewReader(string(serialized))); err != nil {
			msg.ScheduleStatus = "5.2;Could not update the recipient calendar"
			return
		}
	} else if partStat == "ACCEPTED" {
		// Declined bookings never land on the resource calendar, like CS.
		calendarNode, err := p.server.Tree.NodeForPath(defaultCalendar)
		if err != nil {
			msg.ScheduleStatus = "5.2;Could not find the default calendar"
			return
		}
		coll, ok := calendarNode.(dav.Collection)
		if !ok {
			msg.ScheduleStatus = "5.2;Could not find the default calendar"
			return
		}
		fileName := "calserver-" + randomToken() + ".ics"
		if _, err := coll.CreateFile(fileName, strings.NewReader(string(serialized))); err != nil {
			msg.ScheduleStatus = "5.2;Could not deliver to the default calendar"
			return
		}
	}
	msg.ScheduleStatus = "1.2;Message delivered locally"

	// Generate the REPLY to the organizer, but queue it: we are inside the
	// organizer's PUT (InterceptWrite), so their copy is not yet persisted
	// and delivering now could not update it. AfterWrite flushes the queue.
	oldCal, _ := ical.Decode(preState)
	replies, err := ical.ParseEvent(newObject, p.addressesForPrincipal(principalURI), oldCal)
	if err != nil {
		return
	}
	for _, reply := range replies {
		if reply.Method == "REPLY" {
			p.queueReply(r, reply)
		}
	}
}

// queueReply records a REPLY to deliver once the triggering request's write
// has been persisted.
func (p *SchedulePlugin) queueReply(r *http.Request, reply *ical.Message) {
	v, _ := p.pendingReplies.LoadOrStore(r, &[]*ical.Message{})
	queue := v.(*[]*ical.Message)
	*queue = append(*queue, reply)
}

// AfterWrite implements dav.AfterWriteHook: deliver REPLYs queued during
// this request's InterceptWrite (auto-scheduled resources answering the
// organizer whose object now exists).
func (p *SchedulePlugin) AfterWrite(r *http.Request, path string) {
	v, ok := p.pendingReplies.LoadAndDelete(r)
	if !ok {
		return
	}
	for _, reply := range *v.(*[]*ical.Message) {
		p.deliver(r, reply)
	}
}

// hasConflict reports whether the object's occurrence window overlaps any
// busy period on the principal's calendars, ignoring copies of the same UID.
func (p *SchedulePlugin) hasConflict(principalURI string, data []byte) bool {
	d, err := ical.Denormalize(data)
	if err != nil || d.FirstOccurrence == nil || d.LastOccurrence == nil {
		return false
	}
	start := time.Unix(*d.FirstOccurrence, 0).UTC()
	end := time.Unix(*d.LastOccurrence, 0).UTC()
	if !end.After(start) {
		return false
	}

	_, name := splitPath(principalURI)
	home, err := p.server.Tree.NodeForPath("calendars/" + name)
	if err != nil {
		return false
	}
	coll, ok := home.(dav.Collection)
	if !ok {
		return false
	}
	children, err := coll.Children()
	if err != nil {
		return false
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
			if od, err := ical.Denormalize(obj.CalendarData); err == nil && od.UID == d.UID {
				continue // rescheduling the same booking is not a conflict
			}
			payloads = append(payloads, obj.CalendarData)
		}
	}
	if len(payloads) == 0 {
		return false
	}
	fb, err := ical.GenerateFreeBusy(payloads, start, end, time.UTC, time.Now())
	if err != nil {
		return false
	}
	return bytes.Contains(fb, []byte("\r\nFREEBUSY"))
}

// setAttendeePartStat sets PARTSTAT (and clears RSVP) on every ATTENDEE
// matching the calendar-user address across the object's components.
func setAttendeePartStat(cal *goical.Calendar, address, partStat string) {
	for _, comp := range cal.Children {
		if comp.Name == goical.CompTimezone {
			continue
		}
		atts := comp.Props["ATTENDEE"]
		for i := range atts {
			if !strings.EqualFold(atts[i].Value, address) {
				continue
			}
			if atts[i].Params == nil {
				atts[i].Params = make(goical.Params)
			}
			atts[i].Params.Set("PARTSTAT", partStat)
			atts[i].Params.Del("RSVP")
		}
	}
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
					if props[i].Params == nil {
						props[i].Params = goical.Params{}
					}
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
