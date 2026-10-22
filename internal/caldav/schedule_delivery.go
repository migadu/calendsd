// Local delivery of iTIP messages: inbox drops, calendar updates, auto-scheduling.

package caldav

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"

	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	"calserver/internal/pathutil"
)

// ExternalDeliverer delivers to non-local recipients (iMip email). Returns
// true if it handled the message (set a schedule status). r is the request
// that triggered delivery; it carries the authenticated principal (and, once
// stashed, the submission credential) used to send as the logged-in user.
type ExternalDeliverer interface {
	DeliverExternal(r *http.Request, msg *ical.Message) bool
}

// AsyncExternalDeliverer is an ExternalDeliverer that can hand the send to a
// background queue and return immediately, for deliveries whose triggering
// request must not block on SMTP (cancels on DELETE, invitations on a
// many-attendee PUT). The credential and message are captured before the
// request returns; the actual send runs in the background. objectPath is the
// stored object whose SCHEDULE-STATUS the terminal outcome is written back onto
// ("" when nothing is stored — a DELETE's cancels, a resource REPLY); the
// deliverer owns the write-back (see IMipPlugin.DeliverExternalAsync).
// QueuesExternalSends reports whether async delivery will actually offload the
// send; a caller must not defer a send when it returns false.
type AsyncExternalDeliverer interface {
	DeliverExternalAsync(r *http.Request, msg *ical.Message, objectPath string) bool
	QueuesExternalSends() bool
}

// asyncSendsOffloaded reports whether some external deliverer will move a send
// off the request path via a queue — the precondition for deferring a send to
// AfterWrite. When false, deferring would leave the status stuck at pending, so
// the caller delivers synchronously instead.
func (p *SchedulePlugin) asyncSendsOffloaded() bool {
	for _, plugin := range p.server.Plugins() {
		if ext, ok := plugin.(AsyncExternalDeliverer); ok && ext.QueuesExternalSends() {
			return true
		}
	}
	return false
}

// deliver routes one iTIP message: local delivery first, then any external
// deliverers (iMip). Port of sabre's deliver() + the schedule listeners. When
// async is set, external sends are handed to a background queue so the triggering
// request does not block on SMTP, and the deliverer writes the terminal
// SCHEDULE-STATUS back onto the object at objectPath ("" when nothing is stored)
// once the send settles; local delivery stays synchronous either way (it is a
// fast storage write, and a local attendee should see the change at once).
func (p *SchedulePlugin) deliver(r *http.Request, msg *ical.Message, async bool, objectPath string) {
	if principalForAddress(p.server, msg.Recipient) != "" {
		p.scheduleLocalDelivery(r, msg)
	} else {
		for _, plugin := range p.server.Plugins() {
			if async {
				if ext, ok := plugin.(AsyncExternalDeliverer); ok {
					if ext.DeliverExternalAsync(r, msg, objectPath) {
						break
					}
					continue
				}
			}
			if ext, ok := plugin.(ExternalDeliverer); ok {
				if ext.DeliverExternal(r, msg) {
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
	principalURI := principalForAddress(p.server, msg.Recipient)
	if principalURI == "" {
		msg.ScheduleStatus = "3.7;Could not find principal."
		return
	}
	_, name := pathutil.Split(principalURI)
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
	w := p.pendingWorkFor(r)
	w.replies = append(w.replies, reply)
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

	_, name := pathutil.Split(principalURI)
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

func randomToken() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}
