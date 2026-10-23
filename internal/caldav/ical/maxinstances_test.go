package ical

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Audit 4.5: a finite rule past the instance cap used to get the 3500th
// occurrence's end as lastoccurence, so it vanished from time-range queries
// on both servers past that date. The bound is now conservative: UNTIL when
// the rule has one, else the 2038 sentinel, so the object stays a candidate
// for every window it can occur in (the validator excludes it precisely).
func TestDenormalizeCappedRuleKeepsConservativeBound(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "hostile", "count-5000-daily.ics"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Denormalize(data)
	if err != nil {
		t.Fatalf("COUNT=5000 daily: %v", err)
	}
	if d.LastOccurrence == nil || time.Unix(*d.LastOccurrence, 0).UTC().Before(maxDate) {
		t.Errorf("lastoccurence = %v, want the 2038 sentinel, not a truncated bound", d.LastOccurrence)
	}

	// 5000 monthly instances from 2000: the 457th reaches 2038, and sabre's
	// loop stops there and stores the sentinel.
	data = []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:long-monthly\r\nDTSTAMP:20000101T000000Z\r\n" +
		"DTSTART:20000101T100000Z\r\nDTEND:20000101T110000Z\r\nRRULE:FREQ=MONTHLY;COUNT=5000\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n")
	d, err = Denormalize(data)
	if err != nil {
		t.Fatalf("monthly COUNT=5000 from 2000: %v", err)
	}
	if d.LastOccurrence == nil || time.Unix(*d.LastOccurrence, 0).UTC().Before(maxDate) {
		t.Errorf("lastoccurence = %v, want at or past the 2038 sentinel", d.LastOccurrence)
	}
	// Just under the cap still computes an exact bound.
	data = []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:ok\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260101T100000Z\r\nDTEND:20260101T110000Z\r\nRRULE:FREQ=DAILY;COUNT=3500\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n")
	d, err = Denormalize(data)
	if err != nil {
		t.Fatalf("daily COUNT=3500: %v", err)
	}
	want := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC).AddDate(0, 0, 3499)
	if d.LastOccurrence == nil || !time.Unix(*d.LastOccurrence, 0).UTC().Equal(want) {
		t.Errorf("lastoccurence = %v, want %v", d.LastOccurrence, want)
	}
}
