package ical

import (
	"testing"
	"time"
)

// TestRecurringTimeRangeBeyondOldCap covers the fix for the false negative
// where a long-running rule whose matching window lies past the 3500th
// occurrence was reported as "no match". A daily event since 2000 is ~9100
// instances deep by 2025; the query must still match, as sabre's uncapped
// iterator does.
func TestRecurringTimeRangeBeyondOldCap(t *testing.T) {
	data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:daily\r\nDTSTAMP:20000101T000000Z\r\n" +
		"DTSTART:20000101T090000Z\r\nDTEND:20000101T100000Z\r\n" +
		"RRULE:FREQ=DAILY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	comp := cal.Events()[0].Component

	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	in, err := EventInTimeRange(comp, start, end, time.UTC)
	if err != nil {
		t.Fatalf("EventInTimeRange: %v", err)
	}
	if !in {
		t.Error("daily-since-2000 event must match a 2025 window (regression: old 3500-cap false negative)")
	}
}

// TestRecurrenceCapPreventsDoS ensures a dense open-ended rule cannot pin a
// core: a FREQ=SECONDLY event queried over an unbounded range must return
// promptly (bounded by maxRecurrences), not iterate for billions of steps.
func TestRecurrenceCapPreventsDoS(t *testing.T) {
	data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:dos-1\r\nDTSTAMP:20000101T000000Z\r\n" +
		"DTSTART:20000101T000000Z\r\nDTEND:20000101T000010Z\r\n" +
		"RRULE:FREQ=SECONDLY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	comp := cal.Events()[0].Component

	// An open-ended range (sabre's TimeRange defaults: 1900..3000).
	start := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := EventInTimeRange(comp, start, end, time.UTC); err != nil {
			t.Errorf("EventInTimeRange: %v", err)
		}
		// The event's own window (2000) overlaps the range, so the match
		// is found on the first occurrence; the cap matters for the
		// non-matching case, exercised via denormalize below.
		if _, err := Denormalize(data); err != nil {
			t.Errorf("Denormalize: %v", err)
		}
		if _, err := Expand(cal, start, end, time.UTC); err != nil {
			t.Errorf("Expand: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recurrence iteration did not terminate within 5s — the DoS cap is not working")
	}
}
