package caldav

import (
	"encoding/xml"
	"strings"

	goical "github.com/emersion/go-ical"

	"github.com/migadu/calendsd/internal/caldav/ical"
	"github.com/migadu/calendsd/internal/dav"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
)

// DefaultMaxAttendees caps the distinct ATTENDEEs one calendar object may
// schedule ([scheduling] max_attendees). Every attendee is one iTIP message
// and, for an external address, one email, so an uncapped PUT or DELETE was
// a mail-amplification lever for any authenticated user (audit 4.4). The
// value equals the group-expansion cap so one limit covers both paths.
const DefaultMaxAttendees = maxExpandedAttendees

// maxAttendeesPrecondition is RFC 4791 §5.3.2.1's name for the limit; it is
// also advertised as the CALDAV:max-attendees-per-instance property so a
// client can learn the cap before hitting it.
var maxAttendeesPrecondition = xml.Name{Space: davxml.NSCalDAV, Local: "max-attendees-per-instance"}

func effectiveMaxAttendees(configured int) int {
	if configured > 0 {
		return configured
	}
	return DefaultMaxAttendees
}

// distinctAttendees counts the distinct ATTENDEE addresses across every
// VEVENT of the object (master plus overrides), case-insensitively.
func distinctAttendees(cal *goical.Calendar) int {
	seen := map[string]bool{}
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		for _, a := range comp.Props.Values("ATTENDEE") {
			seen[strings.ToLower(a.Value)] = true
		}
	}
	return len(seen)
}

// errTooManyAttendees is the PUT-time refusal: 403 with the RFC precondition.
func errTooManyAttendees(count, limit int) error {
	e := dav.ErrForbidden("The object has %d attendees; this server schedules at most %d per object (CALDAV:max-attendees-per-instance)", count, limit)
	e.Precondition = []xml.Name{maxAttendeesPrecondition}
	return e
}

// attendeesOverCap returns the attendee addresses (lower-cased) beyond the
// first limit distinct ones, in document order. When mark is set, each such
// ATTENDEE property gets SCHEDULE-STATUS=5.3, the same way the group
// expansion marks members it could not invite, so the organizer can see who
// was not scheduled. This is the backstop behind the PUT-time check for
// objects that predate the cap, came from sabre, or grew past it through
// group expansion (which runs after the PUT-time check).
func attendeesOverCap(cal *goical.Calendar, limit int, mark bool) map[string]bool {
	allowed := map[string]bool{}
	over := map[string]bool{}
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		atts := comp.Props.Values("ATTENDEE")
		for i := range atts {
			addr := strings.ToLower(atts[i].Value)
			if allowed[addr] {
				continue
			}
			if !over[addr] && len(allowed) < limit {
				allowed[addr] = true
				continue
			}
			over[addr] = true
			if mark {
				if atts[i].Params == nil {
					atts[i].Params = goical.Params{}
				}
				atts[i].Params.Set("SCHEDULE-STATUS", "5.3")
			}
		}
		if mark && len(atts) > 0 {
			comp.Props["ATTENDEE"] = atts
		}
	}
	return over
}

// withoutRecipients drops the messages addressed to the over-cap attendees.
func withoutRecipients(messages []*ical.Message, over map[string]bool) []*ical.Message {
	if len(over) == 0 {
		return messages
	}
	kept := messages[:0]
	for _, m := range messages {
		if !over[strings.ToLower(m.Recipient)] {
			kept = append(kept, m)
		}
	}
	return kept
}

// isOrganizerOf reports whether the object is scheduled by one of the
// principal's addresses: it carries no ORGANIZER (a plain object, which the
// cap still bounds in case one is added) or its ORGANIZER is one of them.
func isOrganizerOf(cal *goical.Calendar, addresses []string) bool {
	org := organizerAddress(cal)
	if org == "" {
		return true
	}
	for _, a := range addresses {
		if strings.EqualFold(a, org) {
			return true
		}
	}
	return false
}

// checkAttendeeCap is the PUT-time refusal, applied by the scheduling plugin
// to the organizer's copy before any message is built.
func checkAttendeeCap(cal *goical.Calendar, limit int) error {
	if n := distinctAttendees(cal); n > limit {
		return errTooManyAttendees(n, limit)
	}
	return nil
}
