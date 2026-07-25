package ical

import (
	"strings"
	"testing"
)

func findMsg(msgs []*Message, recipient string) *Message {
	for _, m := range msgs {
		if strings.EqualFold(m.Recipient, recipient) {
			return m
		}
	}
	return nil
}

func body(t *testing.T, m *Message) string {
	t.Helper()
	// Unfold so RECURRENCE-ID/EXDATE checks aren't split across lines.
	return strings.ReplaceAll(string(EncodeLenient(m.Calendar)), "\r\n ", "")
}

func countVEVENT(s string) int { return strings.Count(s, "BEGIN:VEVENT") }

func wrapCal(parts ...[]string) string {
	out := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//t//t//EN"}
	for _, p := range parts {
		out = append(out, p...)
	}
	out = append(out, "END:VCALENDAR", "")
	return strings.Join(out, "\r\n")
}

// TestParseEventPerInstanceOrganizer: an attendee invited to the master but
// excluded from an override gets a REQUEST whose master carries an EXDATE for
// the excluded occurrence and only the master VEVENT; an attendee invited only
// to the override gets just that instance.
func TestParseEventPerInstanceOrganizer(t *testing.T) {
	master := []string{
		"BEGIN:VEVENT", "UID:r1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260101T100000Z", "DURATION:PT1H", "RRULE:FREQ=DAILY;COUNT=3",
		"SUMMARY:Series",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=Org;PARTSTAT=ACCEPTED:mailto:org@example.org",
		"ATTENDEE;CN=A;PARTSTAT=NEEDS-ACTION:mailto:a@example.org",
		"END:VEVENT",
	}
	override := []string{
		"BEGIN:VEVENT", "UID:r1", "DTSTAMP:20260101T000000Z",
		"RECURRENCE-ID:20260102T100000Z", "DTSTART:20260102T100000Z", "DURATION:PT1H",
		"SUMMARY:Series (day 2)",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=Org;PARTSTAT=ACCEPTED:mailto:org@example.org",
		"ATTENDEE;CN=B;PARTSTAT=NEEDS-ACTION:mailto:b@example.org",
		"END:VEVENT",
	}
	newCal, err := Decode([]byte(wrapCal(master, override)))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(newCal, []string{"mailto:org@example.org"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	a := findMsg(msgs, "mailto:a@example.org")
	if a == nil || a.Method != "REQUEST" {
		t.Fatalf("A message = %+v, want REQUEST", a)
	}
	ab := body(t, a)
	if !strings.Contains(ab, "EXDATE") || !strings.Contains(ab, "20260102T100000Z") {
		t.Errorf("A's REQUEST should EXDATE the day-2 override:\n%s", ab)
	}
	if strings.Contains(ab, "b@example.org") {
		t.Errorf("A's REQUEST leaked the override it is not part of:\n%s", ab)
	}
	if n := countVEVENT(ab); n != 1 {
		t.Errorf("A should get only the master, got %d VEVENTs:\n%s", n, ab)
	}

	b := findMsg(msgs, "mailto:b@example.org")
	if b == nil || b.Method != "REQUEST" {
		t.Fatalf("B message = %+v, want REQUEST", b)
	}
	bb := body(t, b)
	if !strings.Contains(bb, "RECURRENCE-ID") || !strings.Contains(bb, "20260102T100000Z") {
		t.Errorf("B's REQUEST should carry the override instance:\n%s", bb)
	}
	if n := countVEVENT(bb); n != 1 {
		t.Errorf("B should get only the override, got %d:\n%s", n, bb)
	}
}

// TestParseEventPerInstanceReply: an attendee who declines only one override
// replies with a single VEVENT for that RECURRENCE-ID + PARTSTAT.
func TestParseEventPerInstanceReply(t *testing.T) {
	master := []string{
		"BEGIN:VEVENT", "UID:r2", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260101T100000Z", "DURATION:PT1H", "RRULE:FREQ=DAILY;COUNT=3",
		"SUMMARY:Series",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org",
		"END:VEVENT",
	}
	overrideOld := []string{
		"BEGIN:VEVENT", "UID:r2", "DTSTAMP:20260101T000000Z",
		"RECURRENCE-ID:20260102T100000Z", "DTSTART:20260102T100000Z", "DURATION:PT1H",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org",
		"END:VEVENT",
	}
	overrideNew := []string{
		"BEGIN:VEVENT", "UID:r2", "DTSTAMP:20260101T000000Z",
		"RECURRENCE-ID:20260102T100000Z", "DTSTART:20260102T100000Z", "DURATION:PT1H",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=A;PARTSTAT=DECLINED:mailto:a@example.org",
		"END:VEVENT",
	}
	oldCal, _ := Decode([]byte(wrapCal(master, overrideOld)))
	newCal, _ := Decode([]byte(wrapCal(master, overrideNew)))

	msgs, err := ParseEvent(newCal, []string{"mailto:a@example.org"}, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Method != "REPLY" {
		t.Fatalf("want one REPLY, got %+v", msgs)
	}
	rb := body(t, msgs[0])
	if n := countVEVENT(rb); n != 1 {
		t.Errorf("reply should carry only the changed instance, got %d:\n%s", n, rb)
	}
	if !strings.Contains(rb, "RECURRENCE-ID") || !strings.Contains(rb, "PARTSTAT=DECLINED") {
		t.Errorf("reply should DECLINE the one override:\n%s", rb)
	}
}
