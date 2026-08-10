package ical

// itip_sequence_test.go pins the SEQUENCE handling in organizer-side scheduling
// messages. The bug (fixed in cancelMessage): a CANCEL echoed the stored
// SEQUENCE instead of incrementing it, so a recipient that collates on SEQUENCE
// alone (RFC 5546 §2.1.4) could treat the CANCEL as not-newer than the REQUEST
// it cancels and keep the event. The correct behavior is the REQUEST/CANCEL
// asymmetry: a REQUEST echoes the organizer client's SEQUENCE (the server must
// not desync the stored event from what attendees hold), while a CANCEL bumps
// it so it reliably supersedes the last REQUEST.
//
// Helpers wrapCal/findMsg come from itip_instances_test.go.

import (
	"testing"

	goical "github.com/emersion/go-ical"
)

// seqOf returns the SEQUENCE of the first VEVENT in a message's payload ("" if
// the property is absent).
func seqOf(t *testing.T, m *Message) string {
	t.Helper()
	for _, comp := range m.Calendar.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		if p := comp.Props.Get("SEQUENCE"); p != nil {
			return p.Value
		}
		return ""
	}
	t.Fatalf("no VEVENT in message to %s", m.Recipient)
	return ""
}

// TestCancelIncrementsSequence: deleting an event stored at SEQUENCE:3 must send
// each attendee a CANCEL at SEQUENCE:4, so it beats the REQUEST they hold.
func TestCancelIncrementsSequence(t *testing.T) {
	oldCal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:seq-1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z",
		"SEQUENCE:3", "SUMMARY:Standup",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	// newCal nil = deletion; the acting user is the organizer.
	msgs, err := ParseEvent(nil, []string{"mailto:org@example.org"}, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	m := findMsg(msgs, "mailto:a@example.org")
	if m == nil || m.Method != "CANCEL" {
		t.Fatalf("want a CANCEL to a@example.org; got %+v", m)
	}
	if got := seqOf(t, m); got != "4" {
		t.Errorf("CANCEL SEQUENCE = %q, want 4 (stored 3, incremented)", got)
	}
}

// TestCancelDefaultSequenceIsOne: an event with no SEQUENCE defaults to 0, so
// its CANCEL carries SEQUENCE:1 (exercises the parse of an absent value).
func TestCancelDefaultSequenceIsOne(t *testing.T) {
	oldCal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:seq-0", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z",
		"SUMMARY:Standup",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(nil, []string{"mailto:org@example.org"}, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	m := findMsg(msgs, "mailto:a@example.org")
	if m == nil || m.Method != "CANCEL" {
		t.Fatalf("want a CANCEL to a@example.org; got %+v", m)
	}
	if got := seqOf(t, m); got != "1" {
		t.Errorf("CANCEL SEQUENCE = %q, want 1 (default 0, incremented)", got)
	}
}

// TestRemovedAttendeeCancelBumpsRequestEchoes pins the asymmetry: removing one
// attendee from an event still at SEQUENCE:3 sends the removed attendee a CANCEL
// at 4 (beats their last REQUEST) while the retained attendee's REQUEST still
// echoes 3 — the server must not bump a REQUEST's SEQUENCE, or it would desync
// the stored event from attendees and break REPLY matching.
func TestRemovedAttendeeCancelBumpsRequestEchoes(t *testing.T) {
	base := func(atts ...string) []byte {
		lines := []string{
			"BEGIN:VEVENT", "UID:seq-2", "DTSTAMP:20260101T000000Z",
			"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z",
			"SEQUENCE:3", "SUMMARY:Standup",
			"ORGANIZER;CN=Org:mailto:org@example.org",
		}
		lines = append(lines, atts...)
		lines = append(lines, "END:VEVENT")
		return []byte(wrapCal(lines))
	}
	oldCal, err := Decode(base(
		"ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org",
		"ATTENDEE;CN=B;PARTSTAT=ACCEPTED:mailto:b@example.org",
	))
	if err != nil {
		t.Fatal(err)
	}
	newCal, err := Decode(base(
		"ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org",
	))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(newCal, []string{"mailto:org@example.org"}, oldCal)
	if err != nil {
		t.Fatal(err)
	}

	req := findMsg(msgs, "mailto:a@example.org")
	if req == nil || req.Method != "REQUEST" {
		t.Fatalf("want a REQUEST to the retained attendee a@; got %+v", req)
	}
	if got := seqOf(t, req); got != "3" {
		t.Errorf("REQUEST SEQUENCE = %q, want 3 (echoed, not bumped)", got)
	}

	can := findMsg(msgs, "mailto:b@example.org")
	if can == nil || can.Method != "CANCEL" {
		t.Fatalf("want a CANCEL to the removed attendee b@; got %+v", can)
	}
	if got := seqOf(t, can); got != "4" {
		t.Errorf("CANCEL SEQUENCE = %q, want 4 (incremented)", got)
	}
}
