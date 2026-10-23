package caldav

import (
	"context"
	"encoding/xml"
	"errors"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	goical "github.com/emersion/go-ical"

	"calendsd/internal/acl"
	"calendsd/internal/caldav/ical"
	"calendsd/internal/dav"
	davxml "calendsd/internal/dav/xml"
	"calendsd/internal/pathutil"
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
	// MaxAttendees caps the distinct attendees scheduled per object; zero
	// means DefaultMaxAttendees. Attendees past the cap get SCHEDULE-STATUS
	// 5.3 and no message (audit 4.4).
	MaxAttendees int
	// Now is the clock the auto-schedule conflict horizon is anchored on;
	// nil means time.Now. Tests pin it.
	Now func() time.Time

	// AutoSchedule marks principals that auto-process scheduling messages
	// (Calendar-Server resource/location semantics): REQUESTs are accepted
	// into their default calendar — or declined on a free-busy conflict —
	// without an inbox drop, and a REPLY goes back to the organizer.
	// Nil disables auto-processing (sabre behavior).
	AutoSchedule func(principalURI string) bool

	// pending holds each in-flight request's deferred scheduling work, produced
	// inside a PUT's InterceptWrite but only actionable once that PUT has
	// persisted (AfterWrite): auto-generated REPLYs, and the external
	// invitation/cancellation emails moved off the request's critical path.
	// Keyed by *http.Request. AfterWrite drains and disarms it on the success
	// path; a request-context cleanup drops it if the PUT fails before AfterWrite
	// runs, so a failed PUT can't orphan an entry.
	pending sync.Map

	// writeBackLocks serializes the read-modify-write status write-backs so the
	// concurrent per-attendee updates to one object don't clobber each other.
	// Used when WriteBackLocker is nil (single-process / tests).
	writeBackLocks objectLocks

	// availability caches whether the calendar backend can store working
	// hours (AvailabilityBackend); the tree is fixed once the server runs.
	availabilityOnce sync.Once
	availability     bool

	// WriteBackLocker, when set, serializes the SCHEDULE-STATUS write-back per
	// object. The default (nil → in-process writeBackLocks) only coordinates one
	// process; wire an AdvisoryLocker for a multi-node deployment so durable
	// workers on different nodes patching the same object still serialize.
	WriteBackLocker WriteBackLocker
}

// WriteBackLocker serializes the per-object SCHEDULE-STATUS read-modify-write.
// Lock returns the unlock func. path names the object being patched.
type WriteBackLocker interface {
	Lock(ctx context.Context, path string) (unlock func())
}

// pendingWork is one request's deferred scheduling work plus the handle that
// disarms its orphan cleanup once AfterWrite claims the entry.
type pendingWork struct {
	replies []*ical.Message
	sends   []pendingSend
	stop    func() bool
}

// pendingSend is one deferred external send plus the object path whose
// SCHEDULE-STATUS its outcome is written back onto.
type pendingSend struct {
	msg  *ical.Message
	path string
}

// objectLocks serializes status write-backs per object. It stripes paths into a
// fixed set of mutexes: the same path always maps to the same stripe (so its
// write-backs serialize), unrelated paths seldom collide, and there is no
// per-path map to grow unbounded over the life of the process.
type objectLocks [64]sync.Mutex

func (l *objectLocks) Lock(ctx context.Context, path string) (unlock func()) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(path))
	mu := &l[h.Sum32()%uint32(len(l))]
	mu.Lock()
	return mu.Unlock
}

func (p *SchedulePlugin) PluginName() string { return "caldav-schedule" }

func (p *SchedulePlugin) Init(s *dav.Server) { p.server = s }

func (p *SchedulePlugin) Features() []string {
	f := []string{"calendar-auto-schedule"}
	if p.availabilitySupported() {
		// calendar-availability is RFC 7953's token, inbox-availability
		// CalendarServer's (what Apple and CalDAVTester look for).
		f = append(f, "calendar-availability", "inbox-availability")
	}
	if p.ExpandGroups {
		f = append(f, "calendarserver-group-attendee")
	}
	return f
}

func (p *SchedulePlugin) Methods(ctx context.Context, path string) []string { return []string{"POST"} }

// availabilitySupported reports whether the calendar backend stores working
// hours, so the DAV header only advertises calendar-availability where an
// inbox can actually hold it.
func (p *SchedulePlugin) availabilitySupported() bool {
	p.availabilityOnce.Do(func() {
		// A one-time type probe of the in-memory calendar root: no request
		// and no storage behind it, so no request context either.
		node, err := p.server.Tree.NodeForPath(context.Background(), "calendars")
		if err != nil {
			return
		}
		if root, ok := node.(*CalendarRoot); ok {
			_, p.availability = root.backend.(AvailabilityBackend)
		}
	})
	return p.availability
}

// PropFind provides the scheduling properties on principals, and
// calendar-availability and free-busy-visibility on schedule inboxes.
func (p *SchedulePlugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	ctx := pf.Context()
	cal := func(local string) xml.Name { return xml.Name{Space: davxml.NSCalDAV, Local: local} }

	if inbox, ok := node.(*Inbox); ok {
		p.inboxPropFind(ctx, pf, inbox)
		p.inboxVisibilityPropFind(ctx, pf, inbox)
		return nil
	}

	ph, isPrincipal := node.(acl.PrincipalHolder)
	if !isPrincipal {
		return nil
	}
	_, principalName := pathutil.Split(ph.PrincipalURL())
	homePath := p.server.Href("calendars/"+principalName) + "/"

	pf.Handle(cal("schedule-inbox-URL"), func() any {
		return davxml.Href{homePath + "inbox/"}
	})
	pf.Handle(cal("schedule-outbox-URL"), func() any {
		return davxml.Href{homePath + "outbox/"}
	})
	pf.Handle(cal("calendar-user-address-set"), func() any {
		hrefs := davxml.Href(ph.AlternateURISet())
		hrefs = append(hrefs, p.server.Href(ph.PrincipalURL())+"/")
		return hrefs
	})
	// sabre's Schedule\Plugin reports every principal as an INDIVIDUAL; we
	// additionally surface auto-schedule principals (rooms/resources, matched by
	// the same prefix callback that drives their auto-accept) as ROOM, so a
	// client can distinguish a bookable room from a person in the attendee picker
	// (RFC 5545 §3.2.3 CUTYPE). Nothing on the server keys off this value — the
	// auto-processing decision is the AutoSchedule callback, not this property.
	pf.Handle(cal("calendar-user-type"), func() any {
		if p.AutoSchedule != nil && p.AutoSchedule(ph.PrincipalURL()) {
			return "ROOM"
		}
		// A principal carrying a non-empty {DAV:}group-member-set is a schedulable
		// group (the same test groupMembers uses for expansion), so report GROUP —
		// a client can then flag it in the attendee picker.
		if set, err := ph.GroupMemberSet(ctx); err == nil && len(set) > 0 {
			return "GROUP"
		}
		return "INDIVIDUAL"
	})
	pf.Handle(cal("schedule-default-calendar-URL"), func() any {
		if path := p.defaultCalendarPath(ctx, ph.PrincipalURL()); path != "" {
			return davxml.Href{p.server.Href(path) + "/"}
		}
		return nil
	})
	return nil
}

// InterceptWrite implements dav.WriteInterceptor: PUTs of calendar objects
// run through the iTIP broker; SCHEDULE-STATUS updates are written back into
// the stored object (sabre's calendarObjectChange + $modified).
func (p *SchedulePlugin) InterceptWrite(r *http.Request, path string, data []byte, isNew bool) ([]byte, error) {
	parentPath, _ := pathutil.Split(path)
	parent, err := p.server.Tree.NodeForPath(r.Context(), parentPath)
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
		if node, err := p.server.Tree.NodeForPath(r.Context(), path); err == nil {
			if obj, isObj := node.(*CalendarObject); isObj {
				oldCal, _ = ical.Decode(obj.Data())
			}
		}
	}

	addresses := p.addressesForPrincipal(r.Context(), calNode.Owner())
	// The attendee cap applies to the ORGANIZER's copy only: that is the
	// object whose attendees are messaged. An attendee saving their own copy
	// of a large meeting (delivered to them, not written by them) sends one
	// REPLY, not a fan-out, and must not be refused (audit 4.4).
	limit := effectiveMaxAttendees(p.MaxAttendees)
	organizes := isOrganizerOf(newCal, addresses)
	if organizes {
		if err := checkAttendeeCap(newCal, limit); err != nil {
			return nil, err
		}
	}
	// Schedule-Reply: F stores without scheduling (RFC 6638 §8.1); it is
	// checked after the cap so the advertised limit holds for every stored
	// organizer copy, not only the ones that send.
	if strings.EqualFold(r.Header.Get("Schedule-Reply"), "F") {
		return data, nil
	}
	expanded := false
	if p.ExpandGroups {
		expanded = p.expandGroupAttendees(r.Context(), newCal)
	}
	// Backstop behind the PUT-time refusal: group expansion runs after it,
	// and objects written by sabre or before the cap existed may carry more.
	// Attendees past the cap are marked 5.3, not messaged.
	var over map[string]bool
	if organizes {
		over = attendeesOverCap(newCal, limit, true)
		if len(over) > 0 {
			expanded = true
			slog.Default().Warn("scheduling: attendee cap exceeded; excess attendees not scheduled",
				"path", path, "excess", len(over), "limit", limit)
		}
	}
	messages, err := ical.ParseEvent(newCal, addresses, oldCal)
	if err != nil || len(messages) == 0 {
		// Re-encode only when group expansion rewrote the object; a plain
		// non-scheduling PUT must be stored byte-for-byte (parity).
		if expanded {
			return ical.EncodeLenient(newCal), nil
		}
		return data, nil
	}
	canDefer := p.asyncSendsOffloaded()
	for _, msg := range messages {
		if canDefer && p.deferrableSend(r.Context(), msg) {
			// A significant invitation/cancellation to an external attendee: don't
			// block the PUT on SMTP. Store it pending now; AfterWrite enqueues the
			// send once the object exists and patches the final status back onto
			// it. Enqueuing here instead would race the not-yet-persisted object.
			msg.ScheduleStatus = statusQueued
			p.deferSend(r, msg, path)
			continue
		}
		// Local delivery and non-significant/non-mailto messages are cheap (no
		// network), so they deliver synchronously with an exact status.
		p.deliver(r, msg, false, "")
	}
	// SCHEDULE-FORCE-SEND has done its job (it forced the messages above); remove
	// it so the stored copy doesn't re-fire a send on the next write (RFC 6638).
	ical.StripForceSend(newCal)
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
	node, err := p.server.Tree.NodeForPath(r.Context(), path)
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
	addresses := p.addressesForPrincipal(r.Context(), obj.Owner())
	messages, err := ical.ParseEvent(nil, addresses, oldCal)
	if err != nil {
		return nil
	}
	// The same cap as on write: a stored object with more attendees than the
	// server schedules must not fan out more CANCELs than it sent invites.
	if isOrganizerOf(oldCal, addresses) {
		messages = withoutRecipients(messages, attendeesOverCap(oldCal, effectiveMaxAttendees(p.MaxAttendees), false))
	}
	for _, msg := range messages {
		// Async, no write-back: a delete can fan out one CANCEL per attendee, each
		// a synchronous SMTP submission, which would stall the DELETE response for
		// N × the mailer timeout. The object is being removed, so its
		// SCHEDULE-STATUS is discarded anyway — hand the sends to the queue and
		// let the DELETE return at once.
		p.deliver(r, msg, true, "")
	}
	return nil
}

// AfterWrite implements dav.AfterWriteHook: now that this request's PUT has
// been persisted, deliver the work its InterceptWrite deferred — REPLYs from
// auto-scheduled resources answering the organizer, and the external
// invitation/cancellation emails held off the request's critical path.
func (p *SchedulePlugin) AfterWrite(r *http.Request, path string) {
	v, ok := p.pending.LoadAndDelete(r)
	if !ok {
		return
	}
	w := v.(*pendingWork)
	// We now own the work; disarm the request-context cleanup so it can't also
	// try to drop it. (Claiming the entry already excludes the cleanup, since
	// both go through the same atomic LoadAndDelete/Delete; this just avoids the
	// stray goroutine on the normal path.)
	if w.stop != nil {
		w.stop()
	}
	for _, reply := range w.replies {
		// Async, no write-back: the reply's status is not stored anywhere.
		p.deliver(r, reply, true, "")
	}
	for _, ps := range w.sends {
		// Async with write-back: the object now exists, so the deliverer patches
		// the terminal status onto it (at ps.path) when the background send settles.
		p.deliver(r, ps.msg, true, ps.path)
	}
}

// deferrableSend reports whether msg is a significant scheduling email an
// external (non-local) attendee will actually be sent — the sends worth moving
// off the PUT's critical path. It mirrors DeliverExternal's own send
// precondition (significant, both parties mailto): a message that would instead
// be skipped as insignificant or unsupported must stay synchronous, or its
// deferred object would be stored pending with nothing to ever resolve it.
// Local delivery is likewise cheap and stays synchronous.
func (p *SchedulePlugin) deferrableSend(ctx context.Context, msg *ical.Message) bool {
	return msg.SignificantChange &&
		principalForAddress(ctx, p.server, msg.Recipient) == "" &&
		strings.HasPrefix(msg.Recipient, "mailto:") &&
		strings.HasPrefix(msg.Sender, "mailto:")
}

// deferSend records an external send to be enqueued in AfterWrite. Called only
// from the request goroutine, so the unsynchronized append is safe.
func (p *SchedulePlugin) deferSend(r *http.Request, msg *ical.Message, path string) {
	w := p.pendingWorkFor(r)
	w.sends = append(w.sends, pendingSend{msg: msg, path: path})
}

// pendingWorkFor returns this request's pending-work entry, creating it (and
// arming the orphan cleanup) on first use. The cleanup drops the entry when the
// request context is canceled — i.e. once ServeHTTP returns — so an entry
// stashed in InterceptWrite is not orphaned if the PUT fails before AfterWrite
// drains it. On the success path AfterWrite claims the entry first (still inside
// ServeHTTP, before the context is canceled) and disarms the cleanup. Only ever
// called from the single request goroutine, so the field writes are safe.
func (p *SchedulePlugin) pendingWorkFor(r *http.Request) *pendingWork {
	v, loaded := p.pending.LoadOrStore(r, &pendingWork{})
	w := v.(*pendingWork)
	if !loaded {
		w.stop = context.AfterFunc(r.Context(), func() { p.pending.Delete(r) })
	}
	return w
}

// writeBackLocker returns the configured cross-node locker, or the in-process
// striped mutexes when none is wired.
func (p *SchedulePlugin) writeBackLocker() WriteBackLocker {
	if p.WriteBackLocker != nil {
		return p.WriteBackLocker
	}
	return &p.writeBackLocks
}

// writeBackAttempts bounds the read/patch/conditional-update loop in
// patchScheduleStatus.
const writeBackAttempts = 3

// patchScheduleStatus re-opens the stored object at path and writes status onto
// the ATTENDEE (or ORGANIZER, for a REPLY) matching recipient, persisting it
// through the backend — which, unlike an HTTP PUT, does not re-run the iTIP
// broker, so this cannot trigger another send. It runs on a queue worker after
// the object was persisted, and is best-effort: if the object has since been
// deleted, moved, or no longer names the recipient, it does nothing. The
// per-object lock (writeBackLocker) serializes write-backs among themselves:
// across nodes when WriteBackLocker is an AdvisoryLocker, otherwise only within
// this process (writeBackLocks). A user PUT does not take that lock, so the
// write is conditional on the ETag read here; a mismatch means the user saved
// in between, and the loop re-reads their revision and applies only this
// attendee's parameter to it (audit 4.3).
func (p *SchedulePlugin) patchScheduleStatus(ctx context.Context, path, method, recipient, status string) {
	unlock := p.writeBackLocker().Lock(ctx, path)
	defer unlock()
	msg := &ical.Message{Method: method, Recipient: recipient, ScheduleStatus: status}
	for attempt := 1; attempt <= writeBackAttempts; attempt++ {
		node, err := p.server.Tree.NodeForPath(ctx, path)
		if err != nil {
			return // object deleted; nothing to annotate
		}
		obj, ok := node.(*CalendarObject)
		if !ok {
			return
		}
		expected := obj.ETag()
		cal, err := ical.Decode(obj.Data())
		if err != nil {
			return
		}
		if !hasScheduleTarget(cal, msg) {
			slog.Default().Info("imip schedule-status write-back dropped: participant no longer on the object",
				"path", path, "recipient", recipient, "status", status)
			return
		}
		applyScheduleStatus(cal, []*ical.Message{msg})
		_, err = obj.PutIfMatch(ctx, ical.EncodeLenient(cal), expected)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrETagMismatch) {
			slog.Default().Warn("imip schedule-status write-back failed",
				"path", path, "recipient", recipient, "status", status, "err", err)
			return
		}
	}
	slog.Default().Warn("imip schedule-status write-back gave up: object kept changing",
		"path", path, "recipient", recipient, "status", status, "attempts", writeBackAttempts)
}

// hasScheduleTarget reports whether the message's recipient still appears on
// the object as the property the status would annotate (ATTENDEE, or
// ORGANIZER for a REPLY).
func hasScheduleTarget(cal *goical.Calendar, msg *ical.Message) bool {
	target := "ATTENDEE"
	if msg.Method == "REPLY" {
		target = "ORGANIZER"
	}
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		for _, p := range comp.Props.Values(target) {
			if strings.EqualFold(p.Value, msg.Recipient) {
				return true
			}
		}
	}
	return false
}

// now is the plugin's clock.
func (p *SchedulePlugin) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
