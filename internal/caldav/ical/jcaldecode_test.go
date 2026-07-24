package ical

import (
	"strings"
	"testing"
)

func TestDecodeJCal(t *testing.T) {
	jcal := `["vcalendar",
      [
        ["version", {}, "text", "2.0"],
        ["prodid", {}, "text", "-//Example//EN"]
      ],
      [
        ["vevent",
          [
            ["uid", {}, "text", "abc-123"],
            ["summary", {}, "text", "Lunch; with, notes"],
            ["dtstart", {}, "date-time", "2026-06-01T12:00:00Z"],
            ["dtend", {}, "date-time", "2026-06-01T13:00:00Z"],
            ["rrule", {}, "recur", {"freq": "WEEKLY", "count": 3}]
          ],
          []
        ],
        ["vevent",
          [
            ["uid", {}, "text", "all-day-1"],
            ["dtstart", {}, "date", "2026-06-02"]
          ],
          []
        ]
      ]
    ]`

	cal, err := DecodeJCal([]byte(jcal))
	if err != nil {
		t.Fatalf("DecodeJCal: %v", err)
	}

	// Re-encode and re-parse to confirm the output is valid iCalendar.
	ics := EncodeLenient(cal)
	reparsed, err := Decode(ics)
	if err != nil {
		t.Fatalf("re-parsing produced-iCal failed: %v\n%s", err, ics)
	}

	events := reparsed.Events()
	if len(events) != 2 {
		t.Fatalf("want 2 VEVENTs, got %d", len(events))
	}

	// Timed event: DTSTART must be iCal basic form, not ISO jCal form.
	got := events[0].Props.Get("DTSTART").Value
	if got != "20260601T120000Z" {
		t.Errorf("DTSTART = %q, want 20260601T120000Z", got)
	}
	if rr := events[0].Props.Get("RRULE").Value; !strings.HasPrefix(rr, "FREQ=WEEKLY") || !strings.Contains(rr, "COUNT=3") {
		t.Errorf("RRULE = %q, want FREQ=WEEKLY;...COUNT=3", rr)
	}
	// Text escaping is valid: the raw value is escaped, and a conformant
	// parser unescapes it back to the original via Text().
	if sum, _ := events[0].Props.Get("SUMMARY").Text(); sum != "Lunch; with, notes" {
		t.Errorf("SUMMARY unescaped = %q, want %q", sum, "Lunch; with, notes")
	}

	// All-day event: DATE value with VALUE=DATE param.
	dt := events[1].Props.Get("DTSTART")
	if dt.Value != "20260602" {
		t.Errorf("all-day DTSTART = %q, want 20260602", dt.Value)
	}
	if v := dt.Params.Get("VALUE"); v != "DATE" {
		t.Errorf("all-day DTSTART VALUE param = %q, want DATE", v)
	}
}

func TestDecodeJCalRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		`{"not":"an array"}`,
		`["vtodo", [], []]`, // root must be vcalendar
		`["vcalendar", "notarray", []]`,
		`not json at all`,
	} {
		if _, err := DecodeJCal([]byte(in)); err == nil {
			t.Errorf("expected error for %q, got nil", in)
		}
	}
}

func TestIsJCal(t *testing.T) {
	if !IsJCal([]byte("  \n[\"vcalendar\"")) {
		t.Error("leading-whitespace jCal not detected")
	}
	if IsJCal([]byte("BEGIN:VCALENDAR")) {
		t.Error("iCalendar misdetected as jCal")
	}
}
