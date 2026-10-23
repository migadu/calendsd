package ical

// Fuzz targets for the code every stored object goes through (Decode →
// Denormalize) and for recurrence expansion, which leans on rrule-go. `go
// test` replays the seeds; fuzz with e.g.
//
//	go test ./internal/caldav/ical/ -run '^$' -fuzz FuzzRecurrence -fuzztime 5m

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goical "github.com/emersion/go-ical"
)

// fuzzWindow is the expansion window: a year, so a rule that steps every
// second still hits maxRecurrences rather than running long.
var fuzzWindow = [2]time.Time{
	time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
}

func addICSSeeds(f *testing.F) {
	files, err := filepath.Glob("testdata/*.ics")
	if err != nil || len(files) == 0 {
		f.Fatalf("no seed files: %v", err)
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
}

func FuzzDenormalize(f *testing.F) {
	addICSSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		checkDenormalize(t, data)
		checkExpand(t, data)
	})
}

// FuzzRecurrence puts the fuzzed value in the RRULE of an otherwise fixed
// event, so the fuzzer spends its time on rules rather than on getting
// through the iCalendar parser.
func FuzzRecurrence(f *testing.F) {
	for _, rule := range []string{
		"FREQ=DAILY;COUNT=7",
		"FREQ=WEEKLY;UNTIL=20270101T000000Z;BYDAY=MO,WE,FR",
		"FREQ=MONTHLY;BYDAY=-1FR",
		"FREQ=MONTHLY;BYMONTHDAY=31",
		"FREQ=YEARLY;BYWEEKNO=53;BYDAY=MO",
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29",
		"FREQ=HOURLY;INTERVAL=7;BYSETPOS=1,-1",
		"FREQ=SECONDLY",
		"FREQ=MINUTELY;BYSECOND=0,30;COUNT=100000",
	} {
		f.Add(rule, false)
		f.Add(rule, true)
	}
	f.Fuzz(func(t *testing.T, rule string, allDay bool) {
		if strings.ContainsAny(rule, "\r\n") {
			return // would end the property line and test the parser instead
		}
		start := "DTSTART;TZID=Europe/Berlin:20260326T090000\r\nDTEND;TZID=Europe/Berlin:20260326T100000\r\n"
		if allDay {
			start = "DTSTART;VALUE=DATE:20260326\r\nDTEND;VALUE=DATE:20260327\r\n"
		}
		data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//fuzz//EN\r\nBEGIN:VEVENT\r\n" +
			"UID:fuzz\r\nDTSTAMP:20260101T000000Z\r\n" + start + "RRULE:" + rule + "\r\n" +
			"END:VEVENT\r\nEND:VCALENDAR\r\n")
		checkDenormalize(t, data)
		checkExpand(t, data)
	})
}

// checkDenormalize: no panic, and metadata consistent with the input.
func checkDenormalize(t *testing.T, data []byte) {
	d, err := Denormalize(data)
	if err != nil {
		return
	}
	if d.Size != int64(len(data)) || len(d.ETag) != 32 {
		t.Errorf("size %d etag %q for %d bytes", d.Size, d.ETag, len(data))
	}
	// The bounds are clamped to the epoch. They are not ordered: a DTEND
	// before DTSTART (invalid, but stored) gives last < first, which the
	// time-range check treats the same way (EventInTimeRange).
	for _, b := range []*int64{d.FirstOccurrence, d.LastOccurrence} {
		if b != nil && *b < 0 {
			t.Errorf("occurrence bound %d before the epoch", *b)
		}
	}
}

// checkExpand: no panic, a bounded instance count, and every instance
// starting before the window ends.
func checkExpand(t *testing.T, data []byte) {
	cal, err := Decode(data)
	if err != nil {
		return
	}
	events := 0
	for _, c := range cal.Children {
		if c.Name == goical.CompEvent {
			events++
		}
	}
	out, err := Expand(cal, fuzzWindow[0], fuzzWindow[1], time.UTC)
	if err != nil {
		return
	}
	instances := 0
	for _, c := range out.Children {
		if c.Name != goical.CompEvent {
			continue
		}
		instances++
		p := c.Props.Get(goical.PropDateTimeStart)
		if p == nil {
			continue
		}
		start, err := p.DateTime(time.UTC)
		if err != nil {
			t.Errorf("instance DTSTART %q does not parse: %v", p.Value, err)
			continue
		}
		if !start.Before(fuzzWindow[1]) {
			t.Errorf("instance at %v starts after the window", start)
		}
	}
	if limit := events * (maxRecurrences + 1); instances > limit {
		t.Errorf("%d instances from %d events, more than %d", instances, events, limit)
	}
}
