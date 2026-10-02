package caldav

import (
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"

	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// Working hours (VAVAILABILITY-PLAN.md §3): the inbox's calendar-availability
// property, live and backed by AvailabilityBackend rather than dead property
// storage. Both names address the same value.
var (
	availabilityCS    = xml.Name{Space: "http://calendarserver.org/ns/", Local: "calendar-availability"}
	availabilityRFC   = xml.Name{Space: davxml.NSCalDAV, Local: "calendar-availability"}
	availabilityNames = []xml.Name{availabilityCS, availabilityRFC}
)

// inboxPropFind serves calendar-availability on a schedule inbox.
// PropFind.Handle only resolves names the request asked for, so the
// property stays out of allprop (like CalendarServer) and the backend is only
// read when it is wanted.
func (p *SchedulePlugin) inboxPropFind(pf *dav.PropFind, inbox *Inbox) {
	ab := inbox.availability()
	if ab == nil {
		return
	}
	var (
		loaded bool
		data   []byte
	)
	value := func() any {
		if !loaded {
			loaded = true
			var err error
			if data, err = ab.Availability(inbox.principalURI); err != nil {
				slog.Warn("reading calendar-availability", "principal", inbox.principalURI, "err", err)
			}
		}
		if data == nil {
			return nil // unset: stays 404
		}
		return string(data)
	}
	for _, name := range availabilityNames {
		pf.Handle(name, value)
	}
}

// PrePropPatch implements dav.PrePropPatchHook. On a schedule inbox it claims
// free-busy-visibility (§10) and calendar-availability before property
// storage can take them as dead properties, and validates each value first:
// a rejected value is marked 403 before any PROPPATCH callback runs, so
// nothing in the request is stored (§3.3).
func (p *SchedulePlugin) PrePropPatch(ctx context.Context, path string, node dav.Node, pp *dav.PropPatch) error {
	inbox, ok := node.(*Inbox)
	if !ok {
		return nil
	}
	p.prePropPatchVisibility(inbox, pp)
	var changed []xml.Name
	var value any
	for _, name := range availabilityNames {
		if v, ok := pp.Mutation(name); ok {
			changed = append(changed, name)
			value = v
		}
	}
	if len(changed) == 0 {
		return nil
	}
	ab := inbox.availability()
	reject := func(reason string, err error) {
		slog.Debug("calendar-availability PROPPATCH rejected", "principal", inbox.principalURI, "reason", reason, "err", err)
		pp.SetResultCode(changed, http.StatusForbidden)
	}
	if ab == nil {
		reject("backend has no availability store", nil)
		return nil
	}
	if len(changed) > 1 {
		// Both names are one value; two writes in one request would race.
		reject("both property names in one request", nil)
		return nil
	}

	var data []byte
	switch v := value.(type) {
	case nil:
		// remove
	case string:
		// The XML parser turned the body's CRLFs into LFs; go-ical needs
		// CRLF, and so does every later reader, so store it normalized.
		data = []byte(ical.NormalizeCRLF(v))
		if err := ical.ValidateAvailability(data); err != nil {
			reject("invalid value", err)
			return nil
		}
	default:
		reject("value is XML, not iCalendar text", nil)
		return nil
	}

	pp.Handle(changed, func(map[xml.Name]any) (map[xml.Name]int, bool) {
		var err error
		if data == nil {
			err = ab.DeleteAvailability(inbox.principalURI)
		} else {
			err = ab.SetAvailability(inbox.principalURI, data)
		}
		if err != nil {
			slog.Error("storing calendar-availability", "principal", inbox.principalURI, "err", err)
			return nil, false
		}
		return nil, true
	})
	return nil
}
