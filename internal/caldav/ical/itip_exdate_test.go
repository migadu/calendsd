package ical

import (
	"strings"
	"testing"
)

// organizerExdate invites a@ to the master and b@ alone to the override, so
// a@'s REQUEST excludes the override's occurrence with an EXDATE.
func organizerExdate(t *testing.T, master, overrideRID []string) string {
	t.Helper()
	cal, err := Decode([]byte(wrapCal(
		append(append([]string{"BEGIN:VEVENT", "UID:ex", "DTSTAMP:20260101T000000Z"}, master...),
			"RRULE:FREQ=DAILY;COUNT=5",
			"ORGANIZER:mailto:org@example.org",
			"ATTENDEE:mailto:a@example.org",
			"END:VEVENT"),
		append(append([]string{"BEGIN:VEVENT", "UID:ex", "DTSTAMP:20260101T000000Z"}, overrideRID...),
			"ORGANIZER:mailto:org@example.org",
			"ATTENDEE:mailto:b@example.org",
			"END:VEVENT"),
	)))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(cal, []string{"mailto:org@example.org"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := findMsg(msgs, "mailto:a@example.org")
	if m == nil {
		t.Fatal("no message for a@")
	}
	return body(t, m)
}

// The EXDATE used to take the RECURRENCE-ID's text and the DTSTART's TZID,
// so a UTC RECURRENCE-ID became a TZID'd "…Z" value, a DATE one lost
// VALUE=DATE, and a value merged into an existing EXDATE took that EXDATE's
// params whatever form it was in.
func TestRequestExdateForm(t *testing.T) {
	for _, tc := range []struct {
		name                string
		master, overrideRID []string
		want                string
	}{
		{
			"utc rid, tzid series",
			[]string{"DTSTART;TZID=Europe/Zurich:20260301T090000"},
			[]string{"RECURRENCE-ID:20260302T080000Z", "DTSTART:20260302T080000Z"},
			"EXDATE;TZID=Europe/Zurich:20260302T090000",
		},
		{
			"all-day series",
			[]string{"DTSTART;VALUE=DATE:20260301"},
			[]string{"RECURRENCE-ID;VALUE=DATE:20260302", "DTSTART;VALUE=DATE:20260302"},
			"EXDATE;VALUE=DATE:20260302",
		},
		{
			"merged into a utc exdate",
			[]string{"DTSTART;TZID=Europe/Zurich:20260301T090000", "EXDATE:20260303T080000Z"},
			[]string{"RECURRENCE-ID;TZID=Europe/Zurich:20260302T090000", "DTSTART:20260302T080000Z"},
			"EXDATE:20260303T080000Z,20260302T080000Z",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b := organizerExdate(t, tc.master, tc.overrideRID); !strings.Contains(b, tc.want+"\r\n") {
				t.Errorf("want %s in:\n%s", tc.want, b)
			}
		})
	}
}

// An attendee's REPLY for an occurrence they dropped with an EXDATE, or for
// an override, names it as their copy does. The TZID used to be lost, which
// turned the times floating.
func TestReplyKeepsRecurrenceIDForm(t *testing.T) {
	master := func(extra ...string) []string {
		return append(append([]string{
			"BEGIN:VEVENT", "UID:ex", "DTSTAMP:20260101T000000Z",
			"DTSTART;TZID=Europe/Zurich:20260301T090000",
			"RRULE:FREQ=DAILY;COUNT=5",
			"ORGANIZER:mailto:org@example.org",
			"ATTENDEE;PARTSTAT=ACCEPTED:mailto:a@example.org",
		}, extra...), "END:VEVENT")
	}
	override := func(partStat string) []string {
		return []string{
			"BEGIN:VEVENT", "UID:ex", "DTSTAMP:20260101T000000Z",
			"RECURRENCE-ID;TZID=Europe/Zurich:20260304T090000",
			"DTSTART;TZID=Europe/Zurich:20260304T100000",
			"ORGANIZER:mailto:org@example.org",
			"ATTENDEE;PARTSTAT=" + partStat + ":mailto:a@example.org",
			"END:VEVENT",
		}
	}
	oldCal, err := Decode([]byte(wrapCal(master(), override("ACCEPTED"))))
	if err != nil {
		t.Fatal(err)
	}
	newCal, err := Decode([]byte(wrapCal(
		master("EXDATE;TZID=Europe/Zurich:20260302T090000"), override("DECLINED"))))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(newCal, []string{"mailto:a@example.org"}, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want one REPLY", len(msgs))
	}
	b := body(t, msgs[0])
	for _, want := range []string{
		"RECURRENCE-ID;TZID=Europe/Zurich:20260302T090000",
		"DTSTART;TZID=Europe/Zurich:20260302T090000",
		"RECURRENCE-ID;TZID=Europe/Zurich:20260304T090000",
	} {
		if !strings.Contains(b, want+"\r\n") {
			t.Errorf("want %s in:\n%s", want, b)
		}
	}
}
