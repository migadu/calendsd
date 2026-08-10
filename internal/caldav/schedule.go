package caldav

import (
	"context"
	"encoding/xml"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	goical "github.com/emersion/go-ical"

	"calserver/internal/acl"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/pathutil"
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
	writeBackLocks objectLocks
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

func (l *objectLocks) lock(path string) func() {
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
	_, principalName := pathutil.Split(ph.PrincipalURL())
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
	// sabre's Schedule\Plugin reports every principal as an INDIVIDUAL.
	pf.Handle(cal("calendar-user-type"), func() any { return "INDIVIDUAL" })
	pf.Handle(cal("schedule-default-calendar-URL"), func() any {
		if path := p.defaultCalendarPath(ph.PrincipalURL()); path != "" {
			return davxml.Href{p.server.BaseURI + path + "/"}
		}
		return nil
	})
	return nil
}

// InterceptWrite implements dav.WriteInterceptor: PUTs of calendar objects
// run through the iTIP broker; SCHEDULE-STATUS updates are written back into
// the stored object (sabre's calendarObjectChange + $modified).
func (p *SchedulePlugin) InterceptWrite(r *http.Request, path string, data []byte, isNew bool) ([]byte, error) {
	if strings.EqualFold(r.Header.Get("Schedule-Reply"), "F") {
		return data, nil
	}
	parentPath, _ := pathutil.Split(path)
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
	canDefer := p.asyncSendsOffloaded()
	for _, msg := range messages {
		if canDefer && p.deferrableSend(msg) {
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
		p.deliver(r, msg, false, nil)
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
		// Async, no write-back: a delete can fan out one CANCEL per attendee, each
		// a synchronous SMTP submission, which would stall the DELETE response for
		// N × the mailer timeout. The object is being removed, so its
		// SCHEDULE-STATUS is discarded anyway — hand the sends to the queue and
		// let the DELETE return at once.
		p.deliver(r, msg, true, nil)
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
		p.deliver(r, reply, true, nil)
	}
	for _, ps := range w.sends {
		method, recipient, sendPath := ps.msg.Method, ps.msg.Recipient, ps.path
		// Async with write-back: the object now exists, so the terminal status
		// can be patched onto it when the background send settles.
		p.deliver(r, ps.msg, true, func(status string) {
			p.patchScheduleStatus(sendPath, method, recipient, status)
		})
	}
}

// deferrableSend reports whether msg is a significant scheduling email an
// external (non-local) attendee will actually be sent — the sends worth moving
// off the PUT's critical path. It mirrors DeliverExternal's own send
// precondition (significant, both parties mailto): a message that would instead
// be skipped as insignificant or unsupported must stay synchronous, or its
// deferred object would be stored pending with nothing to ever resolve it.
// Local delivery is likewise cheap and stays synchronous.
func (p *SchedulePlugin) deferrableSend(msg *ical.Message) bool {
	return msg.SignificantChange &&
		p.principalForAddress(msg.Recipient) == "" &&
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

// patchScheduleStatus re-opens the stored object at path and writes status onto
// the ATTENDEE (or ORGANIZER, for a REPLY) matching recipient, persisting it
// through the backend — which, unlike an HTTP PUT, does not re-run the iTIP
// broker, so this cannot trigger another send. It runs on a queue worker after
// the object was persisted, and is best-effort: if the object has since been
// deleted, moved, or no longer names the recipient, it does nothing. The
// per-object lock serializes the read-modify-write so concurrent per-attendee
// write-backs don't clobber one another.
func (p *SchedulePlugin) patchScheduleStatus(path, method, recipient, status string) {
	unlock := p.writeBackLocks.lock(path)
	defer unlock()
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return
	}
	obj, ok := node.(*CalendarObject)
	if !ok {
		return
	}
	cal, err := ical.Decode(obj.Data())
	if err != nil {
		return
	}
	applyScheduleStatus(cal, []*ical.Message{{Method: method, Recipient: recipient, ScheduleStatus: status}})
	if _, err := obj.Put(strings.NewReader(string(ical.EncodeLenient(cal)))); err != nil {
		slog.Default().Warn("imip schedule-status write-back failed",
			"path", path, "recipient", recipient, "status", status, "err", err)
	}
}
