// Package ical wraps go-ical and rrule-go with the calendar-object semantics
// the server needs: parsing, serialization, and the denormalized metadata
// sabre computes on every PUT. It replaces the subset of sabre/vobject the
// CalDAV plugin depends on.
package ical

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	goical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// Decode parses raw iCalendar bytes into a calendar object.
//
// go-ical's line decoder indexes past the end of some malformed lines (a
// parameter with an empty value at the end of the line, "X;P=") instead of
// returning an error. Every PUT body goes through here, so that panic is
// turned into the parse error it should have been.
func Decode(data []byte) (cal *goical.Calendar, err error) {
	defer func() {
		if r := recover(); r != nil {
			cal, err = nil, fmt.Errorf("malformed iCalendar: %v", r)
		}
	}()
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
		if err := checkRecurrence(comp, loc); err != nil {
			return nil, err
		}
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

// checkRecurrence rejects an RRULE that rrule-go would panic on (see
// checkRRule), and one starting before year 1: rrule-go builds its day masks
// when the year changes from the one it last saw, which starts as 0, so a
// rule starting in year 0 never gets them and indexes empty slices.
// Denormalize runs it on every PUT, so such a rule is refused up front
// rather than stored to fail every later expansion.
func checkRecurrence(comp *goical.Component, loc *time.Location) error {
	opt, err := comp.Props.RecurrenceRule()
	if err != nil || opt == nil {
		return nil // no rule, or one RecurrenceSet reports itself
	}
	if err := checkRRule(opt); err != nil {
		return err
	}
	if start, err := comp.Props.DateTime(goical.PropDateTimeStart, loc); err == nil && start.Year() < 1 {
		return fmt.Errorf("recurring DTSTART %v is before year 1", start)
	}
	return nil
}

// checkRRule rejects BYDAY ordinals that rrule-go cannot iterate: it
// indexes its day masks with the nth weekday before checking the result is
// in range, so an ordinal past the period (FREQ=MONTHLY;BYDAY=8FR) panics
// instead of matching nothing. RFC 5545 §3.3.10 allows at most ±53; a month
// holds at most five of a weekday.
func checkRRule(opt *rrule.ROption) error {
	limit := 53
	if opt.Freq == rrule.MONTHLY || (opt.Freq == rrule.YEARLY && len(opt.Bymonth) > 0) {
		limit = 5
	}
	for _, wd := range opt.Byweekday {
		if n := wd.N(); n > limit || n < -limit {
			return fmt.Errorf("RRULE BYDAY ordinal %d is outside ±%d", n, limit)
		}
	}
	return nil
}

// IsRecurring reports whether a component has a recurrence set: an RRULE or
// an RDATE.
func IsRecurring(comp *goical.Component) bool {
	return comp.Props.Get(goical.PropRecurrenceRule) != nil || comp.Props.Get(goical.PropRecurrenceDates) != nil
}

// overridesOf maps the RECURRENCE-ID instants (read in loc) of master's
// overrides, the other components in cal of its type and UID, to them.
// Overrides whose RECURRENCE-ID doesn't parse are left out.
func overridesOf(cal *goical.Calendar, master *goical.Component, loc *time.Location) map[int64]*goical.Component {
	uid := ""
	if p := master.Props.Get(goical.PropUID); p != nil {
		uid = p.Value
	}
	out := map[int64]*goical.Component{}
	for _, child := range cal.Children {
		if child == master || child.Name != master.Name {
			continue
		}
		if p := child.Props.Get(goical.PropUID); p == nil || p.Value != uid {
			continue
		}
		if rid := child.Props.Get(goical.PropRecurrenceID); rid != nil {
			if t, err := rid.DateTime(loc); err == nil {
				out[t.Unix()] = child
			}
		}
	}
	return out
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
	slices.Sort(names)
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
	slices.Sort(paramNames)
	for _, name := range paramNames {
		b.WriteString(";" + name + "=")
		for i, v := range prop.Params[name] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(paramValue(v))
		}
	}
	b.WriteString(":" + prop.Value)
	return b.String()
}

// paramEscaper applies RFC 6868 to the characters a parameter value cannot
// hold as they are.
var paramEscaper = strings.NewReplacer("\r\n", "^n", "\n", "^n", "\r", "^n", `"`, "^'")

// paramValue renders one parameter value: a double quote or line break is
// RFC 6868-encoded, and a value containing ; : or , is quoted. A ^ is left
// alone: go-ical keeps values as written, without decoding RFC 6868, so one
// is already part of an encoding.
func paramValue(v string) string {
	v = paramEscaper.Replace(v)
	if strings.ContainsAny(v, ";:,") {
		return `"` + v + `"`
	}
	return v
}

// writeContentLine folds at 75 octets per RFC 5545 §3.1, counting the
// leading space of a continuation line, and never inside a UTF-8 sequence.
func writeContentLine(buf *bytes.Buffer, line string) {
	const limit = 75
	for n := limit; len(line) > n; n = limit - 1 {
		cut := n
		for !utf8.RuneStart(line[cut]) {
			cut--
		}
		buf.WriteString(line[:cut] + "\r\n ")
		line = line[cut:]
	}
	buf.WriteString(line + "\r\n")
}
