package caldav

import (
	"strings"
	"testing"

	"calendsd/internal/caldav/ical"
)

func TestAttendeesOverCap(t *testing.T) {
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:x\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260901T100000Z\r\n" +
		"ATTENDEE:mailto:a@x\r\nATTENDEE:mailto:b@x\r\nATTENDEE:mailto:B@X\r\nATTENDEE:mailto:c@x\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:x\r\nRECURRENCE-ID:20260908T100000Z\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260908T100000Z\r\n" +
		"ATTENDEE:mailto:c@x\r\nATTENDEE:mailto:d@x\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, err := ical.Decode([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if n := distinctAttendees(cal); n != 4 {
		t.Errorf("distinctAttendees = %d, want 4 (b counted once, d from the override)", n)
	}
	over := attendeesOverCap(cal, 2, true)
	if len(over) != 2 || !over["mailto:c@x"] || !over["mailto:d@x"] {
		t.Errorf("over = %v, want c and d", over)
	}
	out := string(ical.EncodeLenient(cal))
	if strings.Count(out, "SCHEDULE-STATUS=5.3") != 3 { // c twice (master + override), d once
		t.Errorf("want 5.3 on every over-cap ATTENDEE line:\n%s", out)
	}
	if strings.Contains(out, "ATTENDEE;SCHEDULE-STATUS=5.3:mailto:a@x") || strings.Contains(out, "SCHEDULE-STATUS=5.3:mailto:b@x") {
		t.Errorf("an allowed attendee was marked:\n%s", out)
	}
	msgs := []*ical.Message{{Recipient: "mailto:a@x"}, {Recipient: "mailto:C@x"}, {Recipient: "mailto:d@x"}}
	if kept := withoutRecipients(msgs, over); len(kept) != 1 || kept[0].Recipient != "mailto:a@x" {
		t.Errorf("withoutRecipients kept %v", kept)
	}
	if err := checkAttendeeCap(cal, 3); err == nil {
		t.Error("checkAttendeeCap(3) should refuse 4 attendees")
	}
	if err := checkAttendeeCap(cal, 4); err != nil {
		t.Errorf("checkAttendeeCap(4) = %v", err)
	}
}
