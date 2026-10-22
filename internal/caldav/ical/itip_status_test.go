package ical

import (
	"strings"
	"testing"
)

// A cancelled override cancels one occurrence, not the series. The event's
// status used to be whichever VEVENT came last, so cancelling an occurrence
// sent every attendee a CANCEL for the whole event, and the attendee's
// REPLYs were suppressed.
func TestCancelledOverrideIsNotSeriesCancel(t *testing.T) {
	master := []string{
		"BEGIN:VEVENT", "UID:st1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z",
		"RRULE:FREQ=DAILY;COUNT=3", "SEQUENCE:1",
		"ORGANIZER:mailto:org@example.com",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:a@example.com",
		"END:VEVENT",
	}
	override := []string{
		"BEGIN:VEVENT", "UID:st1", "DTSTAMP:20260101T000000Z",
		"RECURRENCE-ID:20260302T090000Z",
		"DTSTART:20260302T090000Z", "DTEND:20260302T100000Z",
		"SEQUENCE:1", "STATUS:CANCELLED",
		"ORGANIZER:mailto:org@example.com",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:a@example.com",
		"END:VEVENT",
	}
	oldCal, err := Decode([]byte(wrapCal(master)))
	if err != nil {
		t.Fatal(err)
	}
	newCal, err := Decode([]byte(wrapCal(master, override)))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(newCal, []string{"mailto:org@example.com"}, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	m := findMsg(msgs, "mailto:a@example.com")
	if m == nil || m.Method != "REQUEST" {
		t.Fatalf("attendee message = %+v, want a REQUEST", m)
	}
	if b := body(t, m); countVEVENT(b) != 2 || !strings.Contains(b, "STATUS:CANCELLED") {
		t.Errorf("REQUEST should carry the master and the cancelled override:\n%s", b)
	}

	// The attendee accepting the series still replies.
	accepted := strings.Replace(wrapCal(master, override),
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:a@example.com\r\nEND:VEVENT\r\nBEGIN:VEVENT",
		"ATTENDEE;PARTSTAT=ACCEPTED:mailto:a@example.com\r\nEND:VEVENT\r\nBEGIN:VEVENT", 1)
	acceptedCal, err := Decode([]byte(accepted))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err = ParseEvent(acceptedCal, []string{"mailto:a@example.com"}, newCal)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Method != "REPLY" {
		t.Fatalf("attendee messages = %d, want one REPLY", len(msgs))
	}
}
