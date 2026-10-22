package ical

import (
	"strings"
	"testing"
	"time"
)

// VTODO alarms used to be resolved like VEVENT ones: RELATED=END read DTEND
// (which a todo never has) and fell back to DTSTART, and a todo without
// DTSTART was an error.
func TestTodoAlarmTrigger(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"end is DUE", []string{
			"DTSTART:20260301T090000Z", "DUE:20260301T170000Z",
			"TRIGGER;RELATED=END:-PT15M",
		}, "20260301T164500Z"},
		{"end is DTSTART+DURATION", []string{
			"DTSTART:20260301T090000Z", "DURATION:PT2H",
			"TRIGGER;RELATED=END:-PT15M",
		}, "20260301T104500Z"},
		{"start is DTSTART", []string{
			"DTSTART:20260301T090000Z", "DUE:20260301T170000Z",
			"TRIGGER:-PT15M",
		}, "20260301T084500Z"},
		{"no DTSTART: DUE", []string{
			"DUE:20260301T170000Z",
			"TRIGGER:-PT15M",
		}, "20260301T164500Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var todo, alarm []string
			for _, l := range tc.lines {
				if strings.HasPrefix(l, "TRIGGER") {
					alarm = append(alarm, l)
				} else {
					todo = append(todo, l)
				}
			}
			lines := append([]string{"BEGIN:VTODO", "UID:td", "DTSTAMP:20260101T000000Z"}, todo...)
			lines = append(lines, "BEGIN:VALARM", "ACTION:DISPLAY")
			lines = append(lines, alarm...)
			lines = append(lines, "END:VALARM", "END:VTODO")
			cal, err := Decode(ics(lines...))
			if err != nil {
				t.Fatal(err)
			}
			parent := cal.Children[0]
			got, err := EffectiveTriggerTime(parent.Children[0], parent, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			if got.Format("20060102T150405Z") != tc.want {
				t.Errorf("trigger = %s, want %s", got.Format("20060102T150405Z"), tc.want)
			}
			in, err := AlarmInTimeRange(parent.Children[0], parent, utc(tc.want), utc(tc.want).Add(time.Minute), time.UTC)
			if err != nil || !in {
				t.Errorf("AlarmInTimeRange = %v, %v; want true", in, err)
			}
		})
	}
}
