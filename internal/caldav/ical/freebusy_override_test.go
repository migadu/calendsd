package ical

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func freeBusyPeriods(t *testing.T, objects ...[]byte) []string {
	t.Helper()
	fb, err := GenerateFreeBusy(objects, utc("20260301T000000Z"), utc("20260310T000000Z"), time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range fbLines(string(fb)) {
		if strings.HasPrefix(line, "FREEBUSY") {
			out = append(out, line)
		}
	}
	return out
}

// A recurring master's overridden occurrences used to stay busy at their
// original time: a moved occurrence was busy twice, and a cancelled or
// transparent one was busy anyway.
func TestFreeBusyHonoursOverrides(t *testing.T) {
	master := []string{
		"BEGIN:VEVENT", "UID:fbo", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z",
		"RRULE:FREQ=DAILY;COUNT=4", "END:VEVENT",
	}
	override := func(day, lines string) []string {
		return append([]string{
			"BEGIN:VEVENT", "UID:fbo", "DTSTAMP:20260101T000000Z",
			"RECURRENCE-ID:202603" + day + "T090000Z",
		}, append(strings.Split(lines, "|"), "END:VEVENT")...)
	}
	data := []byte(wrapCal(master,
		override("02", "DTSTART:20260302T140000Z|DTEND:20260302T150000Z"),
		override("03", "DTSTART:20260303T090000Z|DTEND:20260303T100000Z|STATUS:CANCELLED"),
		override("04", "DTSTART:20260304T090000Z|DTEND:20260304T100000Z|TRANSP:TRANSPARENT"),
	))
	got := freeBusyPeriods(t, data)
	want := []string{
		"FREEBUSY:20260301T090000Z/20260301T100000Z",
		"FREEBUSY:20260302T140000Z/20260302T150000Z",
	}
	if !slices.Equal(got, want) {
		t.Errorf("periods:\n got %q\nwant %q", got, want)
	}
}

// Free-busy didn't resolve non-IANA TZIDs, so one Outlook event failed the
// whole report.
func TestFreeBusyResolvesWindowsTZID(t *testing.T) {
	got := freeBusyPeriods(t, event(
		"DTSTART;TZID=W. Europe Standard Time:20260302T090000",
		"DTEND;TZID=W. Europe Standard Time:20260302T100000",
	))
	if want := []string{"FREEBUSY:20260302T080000Z/20260302T090000Z"}; !slices.Equal(got, want) {
		t.Errorf("periods: got %q, want %q", got, want)
	}
}
