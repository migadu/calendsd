package ical

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goical "github.com/emersion/go-ical"
)

func hostile(t *testing.T, name string) *goical.Calendar {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "hostile", name))
	if err != nil {
		t.Fatal(err)
	}
	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

func firstEvent(t *testing.T, cal *goical.Calendar) *goical.Component {
	t.Helper()
	for _, c := range cal.Children {
		if c.Name == goical.CompEvent {
			return c
		}
	}
	t.Fatal("no VEVENT")
	return nil
}

// A FREQ=SECONDLY rule with no end against a one-year window must answer
// quickly and conservatively (audit 3.1): the scan gives up after its budget
// and reports a match rather than enumerating 31 million instances.
func TestEventInTimeRangeDenseRuleIsBounded(t *testing.T) {
	ev := firstEvent(t, hostile(t, "secondly-forever.ics"))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)

	began := time.Now()
	match, err := EventInTimeRange(ev, start, end, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if !match {
		t.Error("dense rule inside the window should match")
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("EventInTimeRange took %v, want well under a second", took)
	}
}

// The free-busy expansion of a dense rule allocates at most maxRecurrences
// periods and covers the rest of the window with one busy block. Two shapes:
// a rule that started years before the window exhausts the pre-window budget
// and the whole window reads busy; a rule that starts inside the window
// yields a run of real instances and then the fallback block.
func TestEventBusyPeriodsDenseRuleIsBounded(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)

	ev := firstEvent(t, hostile(t, "secondly-forever.ics")) // DTSTART 2020
	began := time.Now()
	periods, err := eventBusyPeriods(ev, start, end, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("eventBusyPeriods took %v", took)
	}
	if len(periods) != 1 || !periods[0].start.Equal(start) || !periods[0].end.Equal(end) {
		t.Errorf("rule from 2020: want one busy period covering the window, got %+v", periods)
	}

	cal, err := Decode([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:dense-inside\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260301T000000Z\r\nDTEND:20260301T000001Z\r\nRRULE:FREQ=SECONDLY\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	periods, err = eventBusyPeriods(firstEvent(t, cal), start, end, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != maxRecurrences+1 {
		t.Fatalf("rule inside the window: got %d periods, want %d instances + 1 fallback", len(periods), maxRecurrences+1)
	}
	if first := periods[0].start; !first.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("first instance at %v, want DTSTART", first)
	}
	if last := periods[len(periods)-1]; !last.end.Equal(end) {
		t.Errorf("fallback period ends %v, want the window end %v", last.end, end)
	}
}

// A long-running daily series whose window lies past its 3500th instance is
// still found exactly: pre-window instances are skipped, not counted (D7).
func TestEventInTimeRangeOldDailySeriesStillExact(t *testing.T) {
	cal, err := Decode([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:old-daily\r\nDTSTAMP:20100101T000000Z\r\n" +
		"DTSTART:20100101T090000Z\r\nDTEND:20100101T093000Z\r\nRRULE:FREQ=DAILY\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	ev := firstEvent(t, cal)
	// 2010 → 2026 is ~5800 instances, past sabre's 3500 cap.
	inside := time.Date(2026, 6, 1, 9, 10, 0, 0, time.UTC)
	match, err := EventInTimeRange(ev, inside, inside.Add(time.Minute), time.UTC)
	if err != nil || !match {
		t.Errorf("window inside an instance: match=%v err=%v", match, err)
	}
	gap := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	match, err = EventInTimeRange(ev, gap, gap.Add(time.Hour), time.UTC)
	if err != nil || match {
		t.Errorf("window between instances: match=%v err=%v, want no match (not a false positive)", match, err)
	}
}

// REPEAT:9223372036854775807 used to spin the alarm loop forever (audit 3.2).
func TestAlarmRepeatMaxIsBounded(t *testing.T) {
	cal := hostile(t, "alarm-repeat-max.ics")
	ev := firstEvent(t, cal)
	var alarm *goical.Component
	for _, c := range ev.Children {
		if c.Name == "VALARM" {
			alarm = c
		}
	}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)

	began := time.Now()
	match, err := AlarmInTimeRange(alarm, ev, start, end, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if match {
		t.Error("alarm a month before the window must not match")
	}
	if took := time.Since(began); took > 100*time.Millisecond {
		t.Errorf("AlarmInTimeRange took %v", took)
	}
	// The recurring-event path shares the loop.
	began = time.Now()
	if alarmRepeatsInRange(alarm, time.Date(2026, 6, 1, 9, 45, 0, 0, time.UTC), start, end) {
		t.Error("recurring alarm path matched outside the window")
	}
	if took := time.Since(began); took > 100*time.Millisecond {
		t.Errorf("alarmRepeatsInRange took %v", took)
	}
}

// Repetitions inside the window still match, and the sabre readings of odd
// REPEAT values are preserved: unparseable or zero means one repetition,
// DURATION without REPEAT means one repetition.
func TestAlarmRepeatParity(t *testing.T) {
	mk := func(valarm string) (*goical.Component, *goical.Component) {
		cal, err := Decode([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
			"BEGIN:VEVENT\r\nUID:a\r\nDTSTAMP:20260101T000000Z\r\n" +
			"DTSTART:20260601T100000Z\r\nDTEND:20260601T110000Z\r\n" +
			"BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:x\r\nTRIGGER:-PT1H\r\n" +
			strings.ReplaceAll(valarm, "\n", "\r\n") +
			"END:VALARM\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		ev := firstEvent(t, cal)
		return ev.Children[0], ev
	}
	// Trigger fires 09:00; repetitions every 10 minutes.
	window := func(h, m int) (time.Time, time.Time) {
		s := time.Date(2026, 6, 1, h, m, 0, 0, time.UTC)
		return s, s.Add(5 * time.Minute)
	}
	for _, tc := range []struct {
		name   string
		valarm string
		h, m   int
		want   bool
	}{
		{"third repetition inside window", "DURATION:PT10M\nREPEAT:3\n", 9, 30, true},
		{"beyond REPEAT count", "DURATION:PT10M\nREPEAT:3\n", 9, 40, false},
		{"DURATION without REPEAT = one repetition", "DURATION:PT10M\n", 9, 10, true},
		{"DURATION without REPEAT, second repetition absent", "DURATION:PT10M\n", 9, 20, false},
		{"REPEAT:abc reads as 1 like PHP", "DURATION:PT10M\nREPEAT:abc\n", 9, 10, true},
		{"REPEAT:0 reads as 1 like PHP", "DURATION:PT10M\nREPEAT:0\n", 9, 10, true},
		{"REPEAT without DURATION is a single trigger", "REPEAT:5\n", 9, 10, false},
		{"zero DURATION cannot stand still", "DURATION:PT0S\nREPEAT:5\n", 9, 0, true},
	} {
		alarm, ev := mk(tc.valarm)
		s, e := window(tc.h, tc.m)
		got, err := AlarmInTimeRange(alarm, ev, s, e, time.UTC)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: AlarmInTimeRange = %v, want %v", tc.name, got, tc.want)
		}
		trigger := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
		if got := alarmRepeatsInRange(alarm, trigger, s, e); got != tc.want {
			t.Errorf("%s: alarmRepeatsInRange = %v, want %v", tc.name, got, tc.want)
		}
	}
}
