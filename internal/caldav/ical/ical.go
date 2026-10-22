// Package ical wraps go-ical and rrule-go with the calendar-object semantics
// the server needs: parsing, serialization, and the denormalized metadata
// sabre computes on every PUT. It replaces the subset of sabre/vobject the
// CalDAV plugin depends on.
package ical

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// Decode parses raw iCalendar bytes into a calendar object.
func Decode(data []byte) (*goical.Calendar, error) {
	return goical.NewDecoder(bytes.NewReader(data)).Decode()
}

// recurrenceSet builds a component's recurrence set, first splitting any
// multi-value (comma-separated) EXDATE/RDATE property into repeated
// single-value properties. go-ical's RecurrenceSet calls Prop.DateTime on each
// EXDATE/RDATE property directly, which errors on the comma list form that
// RFC 5545 permits (EXDATE:...Z,...Z). The original component is left
// untouched: only a shallow copy with rewritten date-list props is passed on.
func recurrenceSet(comp *goical.Component, loc *time.Location) (*rrule.Set, error) {
	needsSplit := false
	for _, name := range []string{goical.PropExceptionDates, goical.PropRecurrenceDates} {
		for _, p := range comp.Props[name] {
			if strings.Contains(p.Value, ",") {
				needsSplit = true
			}
		}
	}
	if !needsSplit {
		return componentSet(comp, loc)
	}
	shallow := &goical.Component{Name: comp.Name, Props: goical.Props{}, Children: comp.Children}
	for name, props := range comp.Props {
		if name != goical.PropExceptionDates && name != goical.PropRecurrenceDates {
			shallow.Props[name] = props
			continue
		}
		var split []goical.Prop
		for _, p := range props {
			for _, v := range strings.Split(p.Value, ",") {
				cp := cloneProp(p)
				cp.Value = v
				split = append(split, cp)
			}
		}
		shallow.Props[name] = split
	}
	return componentSet(shallow, loc)
}

// componentSet is go-ical's RecurrenceSet, plus the set of an RDATE-only
// component, which go-ical treats as not recurring: DTSTART and the RDATEs,
// minus the EXDATEs (RFC 5545 §3.8.5.2). It is nil for a component that does
// not recur.
func componentSet(comp *goical.Component, loc *time.Location) (*rrule.Set, error) {
	if comp.Props.Get(goical.PropRecurrenceRule) != nil || comp.Props.Get(goical.PropRecurrenceDates) == nil {
		return comp.RecurrenceSet(loc)
	}
	dtstart, err := comp.Props.DateTime(goical.PropDateTimeStart, loc)
	if err != nil {
		return nil, fmt.Errorf("parsing DTSTART: %w", err)
	}
	set := &rrule.Set{}
	set.RDate(dtstart)
	for _, p := range comp.Props[goical.PropRecurrenceDates] {
		t, err := p.DateTime(loc)
		if err != nil {
			return nil, fmt.Errorf("parsing RDATE: %w", err)
		}
		set.RDate(t)
	}
	for _, p := range comp.Props[goical.PropExceptionDates] {
		t, err := p.DateTime(loc)
		if err != nil {
			return nil, fmt.Errorf("parsing EXDATE: %w", err)
		}
		set.ExDate(t)
	}
	return set, nil
}

// IsRecurring reports whether a component has a recurrence set: an RRULE or
// an RDATE.
func IsRecurring(comp *goical.Component) bool {
	return comp.Props.Get(goical.PropRecurrenceRule) != nil || comp.Props.Get(goical.PropRecurrenceDates) != nil
}

// ComponentType returns the first non-VTIMEZONE component name (VEVENT, VTODO,
// VJOURNAL, …), or "" if the calendar has none. This is the type CalDAV
// filters and validates against supported-calendar-component-set.
func ComponentType(cal *goical.Calendar) string {
	for _, child := range cal.Children {
		if child.Name != goical.CompTimezone {
			return child.Name
		}
	}
	return ""
}

// NormalizeCRLF restores CRLF line endings, undoing the \r-stripping the
// PROPFIND calendar-data handler applies before XML output. go-ical's parser
// requires CRLF.
func NormalizeCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// Encode serializes a calendar object to iCalendar bytes, enforcing go-ical's
// component validation (DTSTAMP required on VEVENT, etc.).
func Encode(cal *goical.Calendar) ([]byte, error) {
	var buf bytes.Buffer
	if err := goical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeLenient serializes without validation, like sabre/vobject: real-world
// objects (and sabre's own test fixtures) omit required properties such as
// DTSTAMP, and the server must round-trip them anyway. Property order is
// name-sorted (go-ical's prop map loses source order).
func EncodeLenient(cal *goical.Calendar) []byte {
	var buf bytes.Buffer
	encodeComponent(&buf, cal.Component)
	return buf.Bytes()
}

func encodeComponent(buf *bytes.Buffer, comp *goical.Component) {
	writeContentLine(buf, "BEGIN:"+comp.Name)
	names := make([]string, 0, len(comp.Props))
	for name := range comp.Props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, prop := range comp.Props[name] {
			writeContentLine(buf, formatProp(&prop))
		}
	}
	for _, child := range comp.Children {
		encodeComponent(buf, child)
	}
	writeContentLine(buf, "END:"+comp.Name)
}

func formatProp(prop *goical.Prop) string {
	var b strings.Builder
	b.WriteString(prop.Name)
	paramNames := make([]string, 0, len(prop.Params))
	for name := range prop.Params {
		paramNames = append(paramNames, name)
	}
	sort.Strings(paramNames)
	for _, name := range paramNames {
		for _, v := range prop.Params[name] {
			b.WriteString(";" + name + "=")
			if strings.ContainsAny(v, ";:,") {
				b.WriteString(`"` + v + `"`)
			} else {
				b.WriteString(v)
			}
		}
	}
	b.WriteString(":" + prop.Value)
	return b.String()
}

// writeContentLine folds at 75 octets per RFC 5545 §3.1.
func writeContentLine(buf *bytes.Buffer, line string) {
	const limit = 75
	for len(line) > limit {
		buf.WriteString(line[:limit] + "\r\n ")
		line = line[limit:]
	}
	buf.WriteString(line + "\r\n")
}
