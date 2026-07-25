package ical

import (
	"strings"
	"testing"
	"time"
)

// ics builds a VCALENDAR body with CRLF line endings from component lines.
func ics(lines ...string) []byte {
	all := append([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//calserver//test//EN",
	}, lines...)
	all = append(all, "END:VCALENDAR", "")
	return []byte(strings.Join(all, "\r\n"))
}

func event(lines ...string) []byte {
	// DTSTAMP is required by go-ical's encoder (and by RFC 5545); sabre is
	// laxer, so objects it stored may lack it — the PUT path must tolerate
	// that on decode, but our fixtures stay spec-clean.
	all := append([]string{"BEGIN:VEVENT", "UID:test-uid-1", "DTSTAMP:20251201T000000Z"}, lines...)
	all = append(all, "END:VEVENT")
	return ics(all...)
}

func ts(t *testing.T, year int, month time.Month, day, hour, min int) int64 {
	t.Helper()
	return time.Date(year, month, day, hour, min, 0, 0, time.UTC).Unix()
}

func checkBounds(t *testing.T, data []byte, wantFirst, wantLast int64) *Denormalized {
	t.Helper()
	d, err := Denormalize(data)
	if err != nil {
		t.Fatalf("Denormalize: %v", err)
	}
	if d.FirstOccurrence == nil || d.LastOccurrence == nil {
		t.Fatalf("occurrence bounds not set: %+v", d)
	}
	if *d.FirstOccurrence != wantFirst {
		t.Errorf("FirstOccurrence = %d (%s), want %d (%s)",
			*d.FirstOccurrence, time.Unix(*d.FirstOccurrence, 0).UTC(),
			wantFirst, time.Unix(wantFirst, 0).UTC())
	}
	if *d.LastOccurrence != wantLast {
		t.Errorf("LastOccurrence = %d (%s), want %d (%s)",
			*d.LastOccurrence, time.Unix(*d.LastOccurrence, 0).UTC(),
			wantLast, time.Unix(wantLast, 0).UTC())
	}
	return d
}

func TestDenormalizeSimpleEvent(t *testing.T) {
	d := checkBounds(t,
		event("DTSTART:20260101T120000Z", "DTEND:20260101T130000Z"),
		ts(t, 2026, 1, 1, 12, 0), ts(t, 2026, 1, 1, 13, 0))
	if d.ComponentType != "VEVENT" {
		t.Errorf("ComponentType = %q, want VEVENT", d.ComponentType)
	}
	if d.UID != "test-uid-1" {
		t.Errorf("UID = %q, want test-uid-1", d.UID)
	}
	if len(d.ETag) != 32 {
		t.Errorf("ETag = %q, want 32-char md5 hex", d.ETag)
	}
}

func TestDenormalizeAllDayNoEnd(t *testing.T) {
	// All-day event without DTEND spans one day (sabre's +1 day rule).
	checkBounds(t,
		event("DTSTART;VALUE=DATE:20260101"),
		ts(t, 2026, 1, 1, 0, 0), ts(t, 2026, 1, 2, 0, 0))
}

func TestDenormalizeDuration(t *testing.T) {
	checkBounds(t,
		event("DTSTART:20260101T120000Z", "DURATION:PT90M"),
		ts(t, 2026, 1, 1, 12, 0), ts(t, 2026, 1, 1, 13, 30))
}

func TestDenormalizePointEvent(t *testing.T) {
	// Timed event without DTEND or DURATION: last == first.
	checkBounds(t,
		event("DTSTART:20260101T120000Z"),
		ts(t, 2026, 1, 1, 12, 0), ts(t, 2026, 1, 1, 12, 0))
}

func TestDenormalizeFiniteRecurrence(t *testing.T) {
	// Weekly, 3 occurrences: Jan 1, 8, 15. Last end = Jan 15 13:00.
	checkBounds(t,
		event("DTSTART:20260101T120000Z", "DTEND:20260101T130000Z",
			"RRULE:FREQ=WEEKLY;COUNT=3"),
		ts(t, 2026, 1, 1, 12, 0), ts(t, 2026, 1, 15, 13, 0))
}

func TestDenormalizeInfiniteRecurrence(t *testing.T) {
	checkBounds(t,
		event("DTSTART:20260101T120000Z", "DTEND:20260101T130000Z",
			"RRULE:FREQ=DAILY"),
		ts(t, 2026, 1, 1, 12, 0), maxDate.Unix())
}

func TestDenormalizeUntilRecurrence(t *testing.T) {
	// UNTIL bounds the rule: occurrences Jan 1..Jan 29 (last start <= Jan 31).
	checkBounds(t,
		event("DTSTART:20260101T120000Z", "DTEND:20260101T130000Z",
			"RRULE:FREQ=WEEKLY;UNTIL=20260131T235959Z"),
		ts(t, 2026, 1, 1, 12, 0), ts(t, 2026, 1, 29, 13, 0))
}

func TestDenormalizeTZID(t *testing.T) {
	// Berlin is UTC+2 in June (CEST): 10:00 local = 08:00 UTC.
	checkBounds(t,
		event("DTSTART;TZID=Europe/Berlin:20260601T100000",
			"DTEND;TZID=Europe/Berlin:20260601T110000"),
		ts(t, 2026, 6, 1, 8, 0), ts(t, 2026, 6, 1, 9, 0))
}

func TestDenormalizeVTodo(t *testing.T) {
	d, err := Denormalize(ics(
		"BEGIN:VTODO", "UID:todo-1", "SUMMARY:Buy milk", "END:VTODO"))
	if err != nil {
		t.Fatalf("Denormalize: %v", err)
	}
	if d.ComponentType != "VTODO" {
		t.Errorf("ComponentType = %q, want VTODO", d.ComponentType)
	}
	// Occurrence bounds are VEVENT-only, like sabre.
	if d.FirstOccurrence != nil || d.LastOccurrence != nil {
		t.Errorf("VTODO must not carry occurrence bounds: %+v", d)
	}
}

func TestDenormalizeSkipsVTimezone(t *testing.T) {
	d, err := Denormalize(ics(
		"BEGIN:VTIMEZONE", "TZID:Europe/Berlin", "BEGIN:STANDARD",
		"DTSTART:19701025T030000", "TZOFFSETFROM:+0200", "TZOFFSETTO:+0100",
		"END:STANDARD", "END:VTIMEZONE",
		"BEGIN:VEVENT", "UID:tz-1", "DTSTART:20260101T120000Z", "END:VEVENT"))
	if err != nil {
		t.Fatalf("Denormalize: %v", err)
	}
	if d.ComponentType != "VEVENT" {
		t.Errorf("ComponentType = %q, want VEVENT (VTIMEZONE must be skipped)", d.ComponentType)
	}
	if d.UID != "tz-1" {
		t.Errorf("UID = %q, want tz-1", d.UID)
	}
}

func TestDenormalizeNoComponent(t *testing.T) {
	if _, err := Denormalize(ics()); err == nil {
		t.Fatal("Denormalize of empty VCALENDAR should fail")
	}
}

func TestRoundTrip(t *testing.T) {
	src := event(
		"DTSTART;TZID=Europe/Berlin:20260601T100000",
		"DTEND;TZID=Europe/Berlin:20260601T110000",
		"SUMMARY:Team meeting; bring notes\\, please",
		"RRULE:FREQ=WEEKLY;BYDAY=MO,WE",
	)
	cal, err := Decode(src)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	out, err := Encode(cal)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	cal2, err := Decode(out)
	if err != nil {
		t.Fatalf("Decode of re-encoded data: %v", err)
	}

	ev1, ev2 := cal.Events()[0], cal2.Events()[0]
	for _, prop := range []string{"DTSTART", "DTEND", "SUMMARY", "RRULE", "UID"} {
		p1, p2 := ev1.Props.Get(prop), ev2.Props.Get(prop)
		if p1 == nil || p2 == nil {
			t.Fatalf("property %s lost in round-trip", prop)
		}
		if p1.Value != p2.Value {
			t.Errorf("%s value changed: %q -> %q", prop, p1.Value, p2.Value)
		}
		if tz1, tz2 := p1.Params.Get("TZID"), p2.Params.Get("TZID"); tz1 != tz2 {
			t.Errorf("%s TZID param changed: %q -> %q", prop, tz1, tz2)
		}
	}
}

// TestDenormalizeOverrideExtendsLast: an overridden instance moved past the
// RRULE's range extends LastOccurrence (sabre's EventIterator behavior);
// FirstOccurrence stays the master DTSTART.
func TestDenormalizeOverrideExtendsLast(t *testing.T) {
	data := ics(
		"BEGIN:VEVENT", "UID:rec-1", "DTSTAMP:20251201T000000Z",
		"DTSTART:20260101T120000Z", "DTEND:20260101T130000Z",
		"RRULE:FREQ=WEEKLY;COUNT=3", "END:VEVENT",
		// Move the Jan 15 occurrence to Jan 20, ending 15:00.
		"BEGIN:VEVENT", "UID:rec-1", "DTSTAMP:20251201T000000Z",
		"RECURRENCE-ID:20260115T120000Z",
		"DTSTART:20260120T120000Z", "DTEND:20260120T150000Z", "END:VEVENT",
	)
	checkBounds(t, data, ts(t, 2026, 1, 1, 12, 0), ts(t, 2026, 1, 20, 15, 0))
}

// TestDenormalizeOverrideWithinRange: an override that does not extend past
// the normal range leaves LastOccurrence at the master's final occurrence.
func TestDenormalizeOverrideWithinRange(t *testing.T) {
	data := ics(
		"BEGIN:VEVENT", "UID:rec-2", "DTSTAMP:20251201T000000Z",
		"DTSTART:20260101T120000Z", "DTEND:20260101T130000Z",
		"RRULE:FREQ=WEEKLY;COUNT=3", "END:VEVENT",
		"BEGIN:VEVENT", "UID:rec-2", "DTSTAMP:20251201T000000Z",
		"RECURRENCE-ID:20260108T120000Z",
		"DTSTART:20260108T090000Z", "DTEND:20260108T093000Z", "END:VEVENT",
	)
	checkBounds(t, data, ts(t, 2026, 1, 1, 12, 0), ts(t, 2026, 1, 15, 13, 0))
}
