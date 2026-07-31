package caldav

import (
	"encoding/xml"
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
