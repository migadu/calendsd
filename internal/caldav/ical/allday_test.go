package ical

import (
	"strings"
	"testing"
	"time"
)

// A one-day all-day series used to end each occurrence a fixed 24 hours
// after its start, so in a zone leaving DST (Zurich, 25 October 2026, a
// 25-hour day) the occurrence ended at 23:00 the same day: Expand wrote
// DTEND on the start date and free-busy cut the day an hour short.
func TestAllDaySeriesAcrossDSTEnd(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	data := event(
		"DTSTART;VALUE=DATE:20261018", "DTEND;VALUE=DATE:20261019",
		"RRULE:FREQ=WEEKLY;COUNT=2",
	)
	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Expand(cal, utc("20261001T000000Z"), utc("20261101T000000Z"), zurich)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Children) != 2 {
		t.Fatalf("expanded %d instances, want 2", len(out.Children))
	}
	if got := out.Children[1].Props.Get("DTEND").Value; got != "20261026" {
		t.Errorf("Oct 25 instance DTEND = %s, want 20261026", got)
	}

	fb, err := GenerateFreeBusy([][]byte{data}, utc("20261025T000000Z"), utc("20261027T000000Z"), zurich, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "FREEBUSY:20261025T000000Z/20261025T230000Z"; !strings.Contains(string(fb), want) {
		t.Errorf("want %s in:\n%s", want, fb)
	}
}
