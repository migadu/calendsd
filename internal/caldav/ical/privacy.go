package ical

import (
	"bytes"
	"strings"

	goical "github.com/emersion/go-ical"
)

// restrictedKeep is the set of properties preserved on a redacted
// CLASS:CONFIDENTIAL or CLASS:PRIVATE component — enough to place an opaque
// busy block (time and recurrence) without leaking any descriptive detail.
// Everything else (SUMMARY's real value, DESCRIPTION, LOCATION, ATTENDEE,
// ORGANIZER, URL, ATTACH, X-*, …) is dropped.
var restrictedKeep = map[string]bool{
	goical.PropUID:             true,
	"DTSTAMP":                  true,
	goical.PropDateTimeStart:   true,
	goical.PropDateTimeEnd:     true,
	goical.PropDuration:        true,
	goical.PropRecurrenceRule:  true,
	goical.PropRecurrenceDates: true,
	goical.PropExceptionDates:  true,
	goical.PropRecurrenceID:    true,
	"SEQUENCE":                 true,
	"STATUS":                   true,
	"TRANSP":                   true,
	goical.PropClass:           true,
	"CREATED":                  true,
	"LAST-MODIFIED":            true,
}

// RedactRestricted returns iCalendar bytes with every CLASS:CONFIDENTIAL or
// CLASS:PRIVATE VEVENT/VTODO/VJOURNAL reduced to an opaque busy block: only
// time and recurrence properties survive, SUMMARY becomes "Busy", and
// sub-components (VALARM, etc.) are removed. PUBLIC and unclassified components
// pass through untouched.
//
// This enforces the RFC 5545 CONFIDENTIAL and PRIVATE access classes when
// someone reads a calendar they do not own with read-only access: they still
// see that the time is busy (the event keeps DTSTART/DTEND and stays OPAQUE, so
// free/busy is unaffected) but learn nothing about it. Both classes are
// redacted identically and the event is kept rather than dropped, so the
// object stays present in collection listings, multiget, and sync-token deltas
// (removing it would read as a deletion and desynchronize clients). On a decode
// error the input is returned unchanged — stored objects are validated on PUT,
// so this is a fail-safe, not an expected path.
func RedactRestricted(data []byte) []byte {
	// Fast path: without a restricted-class token there is nothing to strip, so
	// skip the decode/encode round-trip that would otherwise touch every read.
	// A stray "PRIVATE"/"CONFIDENTIAL" in free text only costs a no-op
	// round-trip — redactComponent re-checks the actual CLASS value.
	up := bytes.ToUpper(data)
	if !bytes.Contains(up, []byte("CONFIDENTIAL")) && !bytes.Contains(up, []byte("PRIVATE")) {
		return data
	}
	cal, err := Decode(data)
	if err != nil {
		return data
	}
	redacted := false
	for _, child := range cal.Children {
		if redactComponent(child) {
			redacted = true
		}
	}
	if !redacted {
		return data
	}
	return EncodeLenient(cal)
}

// redactComponent strips a component in place when it is a CLASS:CONFIDENTIAL
// or CLASS:PRIVATE calendar component, reporting whether it changed anything.
func redactComponent(comp *goical.Component) bool {
	switch comp.Name {
	case goical.CompEvent, goical.CompToDo, goical.CompJournal:
	default:
		return false // VTIMEZONE and the like are never restricted
	}
	class := comp.Props.Get(goical.PropClass)
	if class == nil {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(class.Value)) {
	case "CONFIDENTIAL", "PRIVATE":
	default:
		return false // PUBLIC or an unknown/extension class: leave untouched
	}
	for name := range comp.Props {
		if !restrictedKeep[name] {
			delete(comp.Props, name)
		}
	}
	comp.Props.SetText(goical.PropSummary, "Busy")
	comp.Children = nil // drop VALARM and any other sub-components
	return true
}
