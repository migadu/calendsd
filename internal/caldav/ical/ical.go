// Package ical wraps go-ical and rrule-go with the calendar-object semantics
// the server needs: parsing, serialization, and the denormalized metadata
// sabre computes on every PUT. It replaces the subset of sabre/vobject the
// CalDAV plugin depends on.
package ical

import (
	"bytes"
	"sort"
	"strings"

	goical "github.com/emersion/go-ical"
)

// Decode parses raw iCalendar bytes into a calendar object.
func Decode(data []byte) (*goical.Calendar, error) {
	return goical.NewDecoder(bytes.NewReader(data)).Decode()
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
