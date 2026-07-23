// Package ical wraps go-ical and rrule-go with the calendar-object semantics
// the server needs: parsing, serialization, and the denormalized metadata
// sabre computes on every PUT. It replaces the subset of sabre/vobject the
// CalDAV plugin depends on.
package ical

import (
	"bytes"

	goical "github.com/emersion/go-ical"
)

// Decode parses raw iCalendar bytes into a calendar object.
func Decode(data []byte) (*goical.Calendar, error) {
	return goical.NewDecoder(bytes.NewReader(data)).Decode()
}

// Encode serializes a calendar object to iCalendar bytes.
func Encode(cal *goical.Calendar) ([]byte, error) {
	var buf bytes.Buffer
	if err := goical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
