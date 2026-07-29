package ical_test

import (
	"strings"
	"testing"

	"calserver/internal/caldav/ical"
)

const confidentialEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:secret-1\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260115T090000Z\r\nDTEND:20260115T100000Z\r\n" +
	"CLASS:CONFIDENTIAL\r\nSUMMARY:Therapy appointment\r\n" +
	"DESCRIPTION:Dr. Smith, 3rd floor\r\nLOCATION:123 Private St\r\n" +
	"ATTENDEE:mailto:someone@example.org\r\nRRULE:FREQ=WEEKLY\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:Therapy in 10m\r\nTRIGGER:-PT10M\r\nEND:VALARM\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

func TestRedactConfidentialStripsDetails(t *testing.T) {
	out := string(ical.RedactConfidential([]byte(confidentialEvent)))

	// Detail leaks the redaction must remove.
	for _, leak := range []string{"Therapy appointment", "Dr. Smith", "123 Private St",
		"someone@example.org", "VALARM", "Therapy in 10m"} {
		if strings.Contains(out, leak) {
			t.Errorf("redacted output still leaks %q:\n%s", leak, out)
		}
	}
	// Busy-block essentials that must survive so free/busy and the grid still
	// place an opaque block.
	for _, keep := range []string{"UID:secret-1", "DTSTART:20260115T090000Z",
		"DTEND:20260115T100000Z", "RRULE:FREQ=WEEKLY", "SUMMARY:Busy"} {
		if !strings.Contains(out, keep) {
			t.Errorf("redacted output dropped required %q:\n%s", keep, out)
		}
	}
}

func TestRedactConfidentialLeavesPublicUntouched(t *testing.T) {
	public := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:pub-1\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260115T090000Z\r\nSUMMARY:Team standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	// No CONFIDENTIAL token: identical bytes returned (fast path, no re-encode).
	if got := string(ical.RedactConfidential([]byte(public))); got != public {
		t.Errorf("public event was modified:\n%s", got)
	}
}

func TestRedactConfidentialMixedCalendar(t *testing.T) {
	mixed := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:pub\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260115T090000Z\r\nSUMMARY:Public meeting\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:conf\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260116T090000Z\r\nCLASS:CONFIDENTIAL\r\nSUMMARY:Secret\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	out := string(ical.RedactConfidential([]byte(mixed)))
	if !strings.Contains(out, "Public meeting") {
		t.Errorf("public event in a mixed calendar was redacted:\n%s", out)
	}
	if strings.Contains(out, "Secret") {
		t.Errorf("confidential event in a mixed calendar leaked:\n%s", out)
	}
}
