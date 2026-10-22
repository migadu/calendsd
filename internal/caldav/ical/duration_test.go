package ical

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		in    string
		days  int
		exact time.Duration
		bad   bool
	}{
		{in: "P1D", days: 1},
		{in: "P2W", days: 14},
		{in: "PT1H30M", exact: 90 * time.Minute},
		{in: "P1DT12H", days: 1, exact: 12 * time.Hour},
		{in: "-P1DT15M", days: -1, exact: -15 * time.Minute},
		{in: "+PT10S", exact: 10 * time.Second},
		{in: "P", bad: true},
		{in: "PT", bad: true},
		{in: "P1H", bad: true},
		{in: "PT1D", bad: true},
		{in: "1D", bad: true},
		{in: "P1DT2HT3M", bad: true},
	} {
		days, exact, err := parseDuration(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseDuration(%q) accepted an invalid duration", tc.in)
			}
			continue
		}
		if err != nil || days != tc.days || exact != tc.exact {
			t.Errorf("parseDuration(%q) = %d, %v, %v; want %d, %v", tc.in, days, exact, err, tc.days, tc.exact)
		}
	}
}

// A one-day DURATION across the spring-forward night ends at the same wall
// time the next day (23 hours later), not 24 hours later.
func TestDurationDayIsNominalAcrossDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	cal, err := Decode([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:p1d\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART;TZID=Europe/Berlin:20260328T100000\r\nDURATION:P1D\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	ev := cal.Children[0]
	// Ends 2026-03-29 10:00 CEST = 08:00Z; 24 hours would be 09:00Z.
	in, err := EventInTimeRange(ev, time.Date(2026, 3, 29, 8, 30, 0, 0, time.UTC), time.Date(2026, 3, 29, 8, 45, 0, 0, time.UTC), berlin)
	if err != nil || in {
		t.Errorf("event still running at 08:30Z on the 29th = %v, %v; want ended at 08:00Z", in, err)
	}
	in, err = EventInTimeRange(ev, time.Date(2026, 3, 29, 7, 30, 0, 0, time.UTC), time.Date(2026, 3, 29, 7, 45, 0, 0, time.UTC), berlin)
	if err != nil || !in {
		t.Errorf("event at 07:30Z on the 29th = %v, %v; want running", in, err)
	}
}
