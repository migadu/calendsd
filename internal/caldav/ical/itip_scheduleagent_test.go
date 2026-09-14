package ical

// Tests for SCHEDULE-AGENT (RFC 6638 §7.1), matching sabre's broker: an
// ATTENDEE marked SCHEDULE-AGENT=CLIENT is excluded from server-side
// scheduling (no REQUEST/CANCEL), and an ORGANIZER marked SCHEDULE-AGENT=CLIENT
// suppresses the server-generated REPLY. Absent (or SERVER) keeps the default
// server-scheduling behavior.
//
// Helpers wrapCal/findMsg come from itip_instances_test.go.

import (
	"strings"
	"testing"
)

func TestScheduleAgentClientAttendeeExcluded(t *testing.T) {
	org := []string{"mailto:org@example.org"}
	newCal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:sa-1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z", "SUMMARY:Planning",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=A;SCHEDULE-AGENT=CLIENT:mailto:a@example.org",
		"ATTENDEE;CN=B;SCHEDULE-AGENT=SERVER:mailto:b@example.org",
		"ATTENDEE;CN=C:mailto:c@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(newCal, org, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := findMsg(msgs, "mailto:a@example.org"); m != nil {
		t.Errorf("SCHEDULE-AGENT=CLIENT attendee got a %s; want no message", m.Method)
	}
	for _, want := range []string{"mailto:b@example.org", "mailto:c@example.org"} {
		if m := findMsg(msgs, want); m == nil || m.Method != "REQUEST" {
			t.Errorf("%s: got %+v, want a REQUEST", want, m)
		}
	}
}

func TestScheduleAgentClientAttendeeNoCancelOnDelete(t *testing.T) {
	org := []string{"mailto:org@example.org"}
	oldCal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:sa-2", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z", "SUMMARY:Planning",
		"ORGANIZER:mailto:org@example.org",
		"ATTENDEE;SCHEDULE-AGENT=CLIENT:mailto:a@example.org",
		"ATTENDEE:mailto:b@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(nil, org, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if m := findMsg(msgs, "mailto:a@example.org"); m != nil {
		t.Errorf("SCHEDULE-AGENT=CLIENT attendee got a %s on delete; want no message", m.Method)
	}
	if m := findMsg(msgs, "mailto:b@example.org"); m == nil || m.Method != "CANCEL" {
		t.Errorf("plain attendee: got %+v, want a CANCEL", m)
	}
}

func TestScheduleAgentClientOrganizerSuppressesReply(t *testing.T) {
	attendee := []string{"mailto:a@example.org"}
	build := func(orgLine, partStat string) []byte {
		return []byte(wrapCal([]string{
			"BEGIN:VEVENT", "UID:sa-3", "DTSTAMP:20260101T000000Z",
			"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z", "SUMMARY:Planning",
			orgLine,
			"ATTENDEE;PARTSTAT=" + partStat + ":mailto:a@example.org",
			"END:VEVENT",
		}))
	}

	// Control: with a server-scheduled organizer the PARTSTAT change replies.
	oldCal, err := Decode(build("ORGANIZER:mailto:org@example.org", "NEEDS-ACTION"))
	if err != nil {
		t.Fatal(err)
	}
	newCal, _ := Decode(build("ORGANIZER:mailto:org@example.org", "ACCEPTED"))
	msgs, err := ParseEvent(newCal, attendee, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if m := findMsg(msgs, "mailto:org@example.org"); m == nil || m.Method != "REPLY" {
		t.Fatalf("server-scheduled organizer: got %+v, want a REPLY", m)
	}

	// SCHEDULE-AGENT=CLIENT (any case) on ORGANIZER: no REPLY is generated.
	oldCal, _ = Decode(build("ORGANIZER;SCHEDULE-AGENT=client:mailto:org@example.org", "NEEDS-ACTION"))
	newCal, _ = Decode(build("ORGANIZER;SCHEDULE-AGENT=client:mailto:org@example.org", "ACCEPTED"))
	msgs, err = ParseEvent(newCal, attendee, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("client-scheduled organizer: got %d message(s), want none", len(msgs))
	}
}

func TestScheduleAgentServerToClientTransitionNoReply(t *testing.T) {
	// The attendee flips their own ATTENDEE to SCHEDULE-AGENT=CLIENT in the
	// same save as a PARTSTAT change: absent from the new matrix, they get no
	// server REPLY — and in particular not one with an empty PARTSTAT.
	attendee := []string{"mailto:a@example.org"}
	oldCal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:sa-5", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z", "SUMMARY:Planning",
		"ORGANIZER:mailto:org@example.org",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:a@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	newCal, _ := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:sa-5", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z", "SUMMARY:Planning",
		"ORGANIZER:mailto:org@example.org",
		"ATTENDEE;PARTSTAT=ACCEPTED;SCHEDULE-AGENT=CLIENT:mailto:a@example.org",
		"END:VEVENT",
	})))
	msgs, err := ParseEvent(newCal, attendee, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("server-to-client transition: got %d message(s), want none", len(msgs))
	}
}

func TestProcessMessageReplyKeepsClientAgentAttendee(t *testing.T) {
	// Applying an incoming REPLY ignores SCHEDULE-AGENT on its ATTENDEE, like
	// sabre's processMessageReply: the filter is a message-generation rule.
	existing, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:sa-6", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z", "SUMMARY:Planning",
		"ORGANIZER:mailto:org@example.org",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:a@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	replyCal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:sa-6", "DTSTAMP:20260102T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z",
		"ORGANIZER:mailto:org@example.org",
		"ATTENDEE;PARTSTAT=ACCEPTED;SCHEDULE-AGENT=CLIENT:mailto:a@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	msg := &Message{
		UID: "sa-6", Method: "REPLY", Component: "VEVENT",
		Sender: "mailto:a@example.org", Recipient: "mailto:org@example.org",
		Calendar: replyCal,
	}
	out, err := ProcessMessage(msg, existing)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("REPLY with SCHEDULE-AGENT=CLIENT attendee was dropped")
	}
	if s := string(EncodeLenient(out)); !strings.Contains(s, "PARTSTAT=ACCEPTED") {
		t.Errorf("REPLY not applied to the existing copy:\n%s", s)
	}
}

func TestScheduleAgentClientAttendeeDeleteNoReply(t *testing.T) {
	// An attendee whose own ATTENDEE carries SCHEDULE-AGENT=CLIENT deletes their
	// copy: the client owns the communication, so no declining REPLY either.
	attendee := []string{"mailto:a@example.org"}
	oldCal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:sa-4", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z", "SUMMARY:Planning",
		"ORGANIZER:mailto:org@example.org",
		"ATTENDEE;PARTSTAT=ACCEPTED;SCHEDULE-AGENT=CLIENT:mailto:a@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(nil, attendee, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("client-scheduled attendee delete: got %d message(s), want none", len(msgs))
	}
}
