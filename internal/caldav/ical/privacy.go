package ical

import (
	"bytes"
	"strings"

	goical "github.com/emersion/go-ical"
)

// confidentialKeep is the set of properties preserved on a redacted
// CLASS:CONFIDENTIAL component — enough to place an opaque busy block (time and
// recurrence) without leaking any descriptive detail. Everything else
// (SUMMARY's real value, DESCRIPTION, LOCATION, ATTENDEE, ORGANIZER, URL,
// ATTACH, X-*, …) is dropped.
var confidentialKeep = map[string]bool{
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

// RedactConfidential returns iCalendar bytes with every CLASS:CONFIDENTIAL
// VEVENT/VTODO/VJOURNAL reduced to an opaque busy block: only time and
// recurrence properties survive, SUMMARY becomes "Busy", and sub-components
// (VALARM, etc.) are removed. Non-confidential components pass through
// untouched.
//
// This enforces the RFC 5545 CONFIDENTIAL access class when someone reads a
// calendar they do not own with read-only access: they still see that the time
// is busy (the event keeps DTSTART/DTEND and stays OPAQUE, so free/busy is
// unaffected) but learn nothing about it. On a decode error the input is
// returned unchanged — stored objects are validated on PUT, so this is a
// fail-safe, not an expected path.
func RedactConfidential(data []byte) []byte {
	// Fast path: without the CONFIDENTIAL token there is nothing to strip, so
	// skip the decode/encode round-trip that would otherwise touch every read.
	if !bytes.Contains(bytes.ToUpper(data), []byte("CONFIDENTIAL")) {
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
// calendar component, reporting whether it changed anything.
func redactComponent(comp *goical.Component) bool {
	switch comp.Name {
	case goical.CompEvent, goical.CompToDo, goical.CompJournal:
	default:
		return false // VTIMEZONE and the like are never confidential
	}
	class := comp.Props.Get(goical.PropClass)
	if class == nil || !strings.EqualFold(strings.TrimSpace(class.Value), "CONFIDENTIAL") {
		return false
	}
	for name := range comp.Props {
		if !confidentialKeep[name] {
			delete(comp.Props, name)
		}
	}
	comp.Props.SetText(goical.PropSummary, "Busy")
	comp.Children = nil // drop VALARM and any other sub-components
	return true
}
