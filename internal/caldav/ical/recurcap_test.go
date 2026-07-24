package ical

import (
	"testing"
	"time"
)

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
