package ical

import (
	"slices"
	"testing"
	"time"
)

// An event that recurs by RDATE alone (no RRULE) used to be treated as a
// single occurrence at DTSTART: Expand, time-range matching, free-busy and
// the stored last occurrence all missed its other dates.
func TestRDateOnlyRecurrence(t *testing.T) {
	data := event(
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z",
		"RDATE:20260305T090000Z,20260310T090000Z",
		"RDATE:20260312T090000Z",
		"EXDATE:20260312T090000Z",
	)
	checkBounds(t, data, ts(t, 2026, 3, 1, 9, 0), ts(t, 2026, 3, 10, 10, 0))

	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	comp := cal.Children[0]
	if !IsRecurring(comp) {
		t.Error("IsRecurring = false for an RDATE-only event")
	}
	in, err := EventInTimeRange(comp, utc("20260310T000000Z"), utc("20260311T000000Z"), time.UTC)
	if err != nil || !in {
		t.Errorf("EventInTimeRange(Mar 10) = %v, %v; want true", in, err)
	}

	got := freeBusyPeriods(t, data)
	want := []string{
		"FREEBUSY:20260301T090000Z/20260301T100000Z",
		"FREEBUSY:20260305T090000Z/20260305T100000Z",
	}
	if !slices.Equal(got, want) { // the window ends on Mar 10
		t.Errorf("free-busy:\n got %q\nwant %q", got, want)
	}

	out, err := Expand(cal, utc("20260304T000000Z"), utc("20260320T000000Z"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var rids []string
	for _, c := range out.Children {
		if c.Props.Get("RDATE") != nil {
			t.Error("expanded instance kept its RDATE")
		}
		rids = append(rids, c.Props.Get("RECURRENCE-ID").Value)
	}
	if want := []string{"20260305T090000Z", "20260310T090000Z"}; !slices.Equal(rids, want) {
		t.Errorf("expanded RECURRENCE-IDs = %q, want %q", rids, want)
	}
}
