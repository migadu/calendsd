package ical

// Tests for SCHEDULE-FORCE-SEND=REQUEST (RFC 6638 §7.1): the organizer resends a
// lost invitation by forcing a send even when nothing significant changed, and
// the parameter is stripped so it doesn't persist and re-fire.
//
// Helpers wrapCal/findMsg come from itip_instances_test.go.

import (
	"strings"
	"testing"
)

func TestForceSendResendsWithoutSignificantChange(t *testing.T) {
	org := []string{"mailto:org@example.org"}
	build := func(attLine string) []byte {
		return []byte(wrapCal([]string{
			"BEGIN:VEVENT", "UID:fs-1", "DTSTAMP:20260101T000000Z",
			"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z", "SEQUENCE:3", "SUMMARY:Standup",
			"ORGANIZER;CN=Org:mailto:org@example.org",
			attLine,
			"END:VEVENT",
		}))
	}
	oldCal, err := Decode(build("ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org"))
	if err != nil {
		t.Fatal(err)
	}

	// Baseline: an identical re-save produces an *insignificant* REQUEST (no send).
	same, _ := Decode(build("ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org"))
	msgs, err := ParseEvent(same, org, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if m := findMsg(msgs, "mailto:a@example.org"); m == nil || m.SignificantChange {
		t.Fatalf("unchanged re-save: got %+v, want a message with SignificantChange=false", m)
	}

	// With SCHEDULE-FORCE-SEND=REQUEST, the same event forces a significant REQUEST.
	forced, _ := Decode(build("ATTENDEE;CN=A;PARTSTAT=ACCEPTED;SCHEDULE-FORCE-SEND=REQUEST:mailto:a@example.org"))
	msgs, err = ParseEvent(forced, org, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	m := findMsg(msgs, "mailto:a@example.org")
	if m == nil || m.Method != "REQUEST" || !m.SignificantChange {
		t.Fatalf("force-send: got %+v, want a REQUEST with SignificantChange=true", m)
	}
}

func TestStripForceSend(t *testing.T) {
	cal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:fs-2", "DTSTAMP:20260101T000000Z",
		"ORGANIZER;SCHEDULE-FORCE-SEND=REQUEST:mailto:org@example.org",
		"ATTENDEE;PARTSTAT=ACCEPTED;SCHEDULE-FORCE-SEND=REQUEST:mailto:a@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	StripForceSend(cal)
	if out := string(EncodeLenient(cal)); strings.Contains(out, "SCHEDULE-FORCE-SEND") {
		t.Errorf("SCHEDULE-FORCE-SEND was not stripped:\n%s", out)
	}
}
