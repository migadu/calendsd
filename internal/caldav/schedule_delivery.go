// Local delivery of iTIP messages: inbox drops, calendar updates, auto-scheduling.

package caldav

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"

	"github.com/migadu/calendsd/internal/caldav/ical"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/pathutil"
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
	for ext := range dav.Hooks[AsyncExternalDeliverer](p.server) {
		if ext.QueuesExternalSends() {
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
	if principalForAddress(r.Context(), p.server, msg.Recipient) != "" {
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
	principalURI := principalForAddress(r.Context(), p.server, msg.Recipient)
	if principalURI == "" {
		msg.ScheduleStatus = "3.7;Could not find principal."
		return
	}
	_, name := pathutil.Split(principalURI)
	homePath := "calendars/" + name

	inboxNode, err := p.server.Tree.NodeForPath(r.Context(), homePath+"/inbox")
	if err != nil {
		msg.ScheduleStatus = "5.2;Could not find local inbox"
		return
	}
	inbox, isInbox := inboxNode.(*Inbox)
	if !isInbox {
		msg.ScheduleStatus = "5.2;Could not find local inbox"
		return
	}
	defaultCalendar := p.defaultCalendarPath(r.Context(), principalURI)
	if defaultCalendar == "" {
		msg.ScheduleStatus = "5.2;Could not find a schedule-default-calendar-URL property"
		return
	}

	// Locate an existing copy by UID across the calendars the recipient
	// OWNS. Shared instances are skipped: a message addressed to the sharee
	// must never write into the sharer's calendar through them (audit 4.2).
	var existingObj *CalendarObject
	home, _ := p.server.Tree.NodeForPath(r.Context(), homePath)
	if coll, ok := home.(dav.Collection); ok {
		children, _ := coll.Children(r.Context())
		for _, child := range children {
			calNode, isCal := child.(*Calendar)
			if !isCal || !calNode.ownedInstance() {
				continue
			}
			if obj, err := calNode.ObjectByUID(r.Context(), msg.UID); err == nil && obj != nil {
				existingObj = obj
				break
			}
		}
	}

	var existingCal *goical.Calendar
	if existingObj != nil {
		existingCal, _ = ical.Decode(existingObj.Data())
	}

	// A REQUEST or CANCEL may only replace or cancel a stored copy that the
	// same organizer owns. Anyone who knows a UID can otherwise overwrite the
	// recipient's own event, or cancel it, by sending a message for that
	// UID. On a mismatch the message still reaches the inbox so the user
	// sees it; only the automatic write is withheld (audit 4.2, D3).
	if existingCal != nil && (msg.Method == "REQUEST" || msg.Method == "CANCEL") &&
		!sameOrganizer(existingCal, msg.Calendar) {
		fileName := "calserver-" + randomToken() + ".ics"
		if _, err := inbox.CreateFile(r.Context(), fileName, strings.NewReader(string(ical.EncodeLenient(msg.Calendar)))); err != nil {
			msg.ScheduleStatus = "5.2;Could not deliver to local inbox"
			return
		}
		msg.ScheduleStatus = statusOrganizerMismatch
		return
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
	fileName := "calendsd-" + randomToken() + ".ics"
	if _, err := inbox.CreateFile(r.Context(), fileName, strings.NewReader(string(ical.EncodeLenient(msg.Calendar)))); err != nil {
		msg.ScheduleStatus = "5.2;Could not deliver to local inbox"
		return
	}

	if newObject != nil {
		serialized := ical.EncodeLenient(newObject)
		if existingObj != nil {
			if _, err := existingObj.Put(r.Context(), strings.NewReader(string(serialized))); err != nil {
				msg.ScheduleStatus = "5.2;Could not update the recipient calendar"
				return
			}
		} else {
			calendarNode, err := p.server.Tree.NodeForPath(r.Context(), defaultCalendar)
			if err != nil {
				msg.ScheduleStatus = "5.2;Could not find the default calendar"
				return
			}
			if coll, ok := calendarNode.(dav.Collection); ok {
				if _, err := coll.CreateFile(r.Context(), fileName, strings.NewReader(string(serialized))); err != nil {
					msg.ScheduleStatus = "5.2;Could not deliver to the default calendar"
					return
				}
			}
		}
	}
	msg.ScheduleStatus = "1.2;Message delivered locally"
}

// statusOrganizerMismatch is the delivery status for a REQUEST or CANCEL whose
// ORGANIZER differs from the one on the recipient's stored copy of that UID.
// 3.x is the "recipient-side, permanent" class alongside 3.7 (unknown
// principal); the message was delivered to the inbox only.
const statusOrganizerMismatch = "3.8;Organizer does not match the stored event; delivered to the inbox only"

// ownedInstance reports whether this calendar view belongs to its principal
// (not a shared instance of someone else's calendar).
func (c *Calendar) ownedInstance() bool {
	return c.Info.ShareAccess == AccessNotShared || c.Info.ShareAccess == AccessSharedOwner
}

// organizerAddress returns the normalized ORGANIZER calendar-user address of
// the first component carrying one: lower-cased, parameters ignored. "" when
// there is none.
func organizerAddress(cal *goical.Calendar) string {
	if cal == nil {
		return ""
	}
	for _, c := range cal.Children {
		if p := c.Props.Get("ORGANIZER"); p != nil {
			return strings.ToLower(strings.TrimSpace(p.Value))
		}
	}
	return ""
}

// sameOrganizer reports whether the incoming message's ORGANIZER is the one
// on the stored copy. A stored copy with no ORGANIZER (the recipient's own,
// unscheduled event) never matches.
func sameOrganizer(stored, incoming *goical.Calendar) bool {
	have := organizerAddress(stored)
	return have != "" && have == organizerAddress(incoming)
}

// autoProcess handles a scheduling message for an auto-schedule principal
// (Calendar-Server resource/location semantics): no inbox drop; REQUESTs are
// stored with PARTSTAT=ACCEPTED — or DECLINED when the booking window
// conflicts with existing events — and a REPLY is sent to the organizer;
// CANCELs remove the copy.
func (p *SchedulePlugin) autoProcess(r *http.Request, msg *ical.Message, principalURI, defaultCalendar string, newObject *goical.Calendar, existingObj *CalendarObject) {
	if msg.Method == "CANCEL" {
		if existingObj != nil {
			if err := existingObj.Delete(r.Context()); err != nil {
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
	if p.hasConflict(r.Context(), principalURI, preState) {
		partStat = "DECLINED"
	}
	setAttendeePartStat(newObject, msg.Recipient, partStat)
	serialized := ical.EncodeLenient(newObject)

	if existingObj != nil {
		if _, err := existingObj.Put(r.Context(), strings.NewReader(string(serialized))); err != nil {
			msg.ScheduleStatus = "5.2;Could not update the recipient calendar"
			return
		}
	} else if partStat == "ACCEPTED" {
		// Declined bookings never land on the resource calendar, like CS.
		calendarNode, err := p.server.Tree.NodeForPath(r.Context(), defaultCalendar)
		if err != nil {
			msg.ScheduleStatus = "5.2;Could not find the default calendar"
			return
		}
		coll, ok := calendarNode.(dav.Collection)
		if !ok {
			msg.ScheduleStatus = "5.2;Could not find the default calendar"
			return
		}
		fileName := "calendsd-" + randomToken() + ".ics"
		if _, err := coll.CreateFile(r.Context(), fileName, strings.NewReader(string(serialized))); err != nil {
			msg.ScheduleStatus = "5.2;Could not deliver to the default calendar"
			return
		}
	}
	msg.ScheduleStatus = "1.2;Message delivered locally"

	// Generate the REPLY to the organizer, but queue it: we are inside the
	// organizer's PUT (InterceptWrite), so their copy is not yet persisted
	// and delivering now could not update it. AfterWrite flushes the queue.
	oldCal, _ := ical.Decode(preState)
	replies, err := ical.ParseEvent(newObject, p.addressesForPrincipal(r.Context(), principalURI), oldCal)
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

// conflictHorizon is how far ahead of a booking's anchor its instances are
// conflict-checked: the same span free-busy answers (audit 4.6). Instances
// of a standing booking beyond it are accepted unchecked; the alternative,
// declining every open-ended booking, is not what a room should do.
const conflictHorizon = maxFreeBusySpanDays * 24 * time.Hour

// hasConflict reports whether any instance of the incoming booking overlaps
// busy time on the principal's calendars, ignoring copies of the same UID.
//
// The check is per instance within a horizon, the way Calendar Server does
// it. The first draft compared the booking's whole occurrence hull — for a
// weekly booking DTSTART→2038 — against the room's free-busy over that hull,
// so a standing booking was declined if the room had anything at all, and it
// expanded every room event across the hull on every booking.
//
// The horizon is anchored at the booking's first instance; for a series that
// has already started it is anchored at now, so a years-old standing booking
// being re-sent is checked for the coming year, not its past.
func (p *SchedulePlugin) hasConflict(ctx context.Context, principalURI string, data []byte) bool {
	d, err := ical.Denormalize(data)
	if err != nil || d.FirstOccurrence == nil || d.LastOccurrence == nil {
		return false
	}
	first := time.Unix(*d.FirstOccurrence, 0).UTC()
	last := time.Unix(*d.LastOccurrence, 0).UTC()
	if !last.After(first) {
		return false
	}
	cal, err := ical.Decode(data)
	if err != nil {
		return false
	}
	anchor := first
	if now := p.now().UTC(); now.After(first) && last.After(now) {
		anchor = now
	}
	horizonEnd := anchor.Add(conflictHorizon)
	if last.Before(horizonEnd) {
		horizonEnd = last
	}
	instances, err := ical.InstancePeriods(cal, anchor, horizonEnd, time.UTC)
	if errors.Is(err, ical.ErrTooManyInstances) {
		return true // too dense to check is a conflict, not a free pass
	}
	if err != nil || len(instances) == 0 {
		return false
	}

	_, name := pathutil.Split(principalURI)
	home, err := p.server.Tree.NodeForPath(ctx, "calendars/"+name)
	if err != nil {
		return false
	}
	coll, ok := home.(dav.Collection)
	if !ok {
		return false
	}
	children, err := coll.Children(ctx)
	if err != nil {
		return false
	}
	var payloads [][]byte
	for _, child := range children {
		calNode, isCal := child.(*Calendar)
		if !isCal || calNode.transparent() {
			continue
		}
		// The backend prefilter (occurrence bounds) keeps a room with years
		// of past bookings from being loaded and expanded on every request.
		objs, err := calNode.QueryObjects(ctx, "VEVENT", anchor, horizonEnd)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			if od, err := ical.Denormalize(obj.Data()); err == nil && od.UID == d.UID {
				continue // rescheduling the same booking is not a conflict
			}
			payloads = append(payloads, obj.Data())
		}
	}
	if len(payloads) == 0 {
		return false
	}
	// The room's busy time once over the horizon, then intersect.
	busy, err := ical.BusyPeriods(payloads, anchor, horizonEnd, time.UTC)
	if err != nil {
		return false
	}
	for _, inst := range instances {
		for _, b := range busy {
			if b.Start.Before(inst.End) && b.End.After(inst.Start) {
				return true
			}
		}
	}
	return false
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
