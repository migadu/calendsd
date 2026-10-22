package ical

import (
	"testing"

	goical "github.com/emersion/go-ical"
)

// organizerCopy is a daily 09:00 Zurich series with an override for Mar 2,
// attendee A still NEEDS-ACTION everywhere.
func organizerCopy() []string {
	return []string{
		"BEGIN:VEVENT", "UID:rp", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=Europe/Zurich:20260301T090000",
		"DTEND;TZID=Europe/Zurich:20260301T100000",
		"RRULE:FREQ=DAILY;COUNT=3",
		"ORGANIZER:mailto:org@example.org",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:a@example.org",
		"END:VEVENT",
		"BEGIN:VEVENT", "UID:rp", "DTSTAMP:20260101T000000Z",
		"RECURRENCE-ID;TZID=Europe/Zurich:20260302T090000",
		"DTSTART;TZID=Europe/Zurich:20260302T110000",
		"DTEND;TZID=Europe/Zurich:20260302T120000",
		"ORGANIZER:mailto:org@example.org",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:a@example.org",
		"END:VEVENT",
	}
}

// applyReply applies a REPLY from A whose VEVENTs carry the given extra lines
// (RECURRENCE-ID, PARTSTAT) to the organizer's copy.
func applyReply(t *testing.T, instances ...[]string) *Message {
	t.Helper()
	var parts [][]string
	for _, lines := range instances {
		ev := []string{"BEGIN:VEVENT", "UID:rp", "DTSTAMP:20260101T000000Z", "ORGANIZER:mailto:org@example.org"}
		ev = append(ev, lines...)
		parts = append(parts, append(ev, "END:VEVENT"))
	}
	reply, err := Decode([]byte(wrapCal(append([][]string{{"METHOD:REPLY"}}, parts...)...)))
	if err != nil {
		t.Fatal(err)
	}
	return &Message{Method: "REPLY", Calendar: reply}
}

func processReply(t *testing.T, msg *Message) *goical.Calendar {
	t.Helper()
	existing, err := Decode([]byte(wrapCal(organizerCopy())))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := ProcessMessage(msg, existing)
	if err != nil || updated == nil {
		t.Fatalf("ProcessMessage = %v, %v", updated, err)
	}
	return updated
}

func partStatAt(t *testing.T, cal *goical.Calendar, rid string) string {
	t.Helper()
	comp := eventByRID(cal, rid)
	if comp == nil {
		t.Fatalf("no VEVENT with RECURRENCE-ID %q", rid)
	}
	ps, _ := attendeePartStat(comp, "a@example.org")
	return ps
}

// A reply for the master only used to be copied onto every override too,
// overwriting the answers given for those occurrences.
func TestReplyMasterNotCopiedToOverrides(t *testing.T) {
	got := processReply(t, applyReply(t, []string{
		"DTSTART;TZID=Europe/Zurich:20260301T090000",
		"ATTENDEE;PARTSTAT=ACCEPTED:mailto:a@example.org",
	}))
	if ps := partStatAt(t, got, ""); ps != "ACCEPTED" {
		t.Errorf("master PARTSTAT = %q, want ACCEPTED", ps)
	}
	if ps := partStatAt(t, got, "20260302T090000"); ps != "NEEDS-ACTION" {
		t.Errorf("override PARTSTAT = %q, want NEEDS-ACTION", ps)
	}
}

// Instances are matched by time: a UTC RECURRENCE-ID finds the override the
// organizer wrote with a TZID. It used to be matched by text and dropped.
func TestReplyMatchesOverrideByTime(t *testing.T) {
	got := processReply(t, applyReply(t, []string{
		"RECURRENCE-ID:20260302T080000Z",
		"ATTENDEE;PARTSTAT=DECLINED:mailto:a@example.org",
	}))
	if ps := partStatAt(t, got, "20260302T090000"); ps != "DECLINED" {
		t.Errorf("override PARTSTAT = %q, want DECLINED", ps)
	}
	if ps := partStatAt(t, got, ""); ps != "NEEDS-ACTION" {
		t.Errorf("master PARTSTAT = %q, want NEEDS-ACTION", ps)
	}
}

// A reply for an occurrence without an override gets one, in the master's
// form; one naming no occurrence of the series changes nothing.
func TestReplyCreatesMissingOverride(t *testing.T) {
	got := processReply(t, applyReply(t,
		[]string{
			"RECURRENCE-ID:20260303T080000Z",
			"ATTENDEE;PARTSTAT=TENTATIVE:mailto:a@example.org",
		},
		[]string{
			"RECURRENCE-ID:20260310T080000Z",
			"ATTENDEE;PARTSTAT=DECLINED:mailto:a@example.org",
		},
	))
	ov := eventByRID(got, "20260303T090000")
	if ov == nil {
		t.Fatal("no override created for Mar 3")
	}
	if ps := partStatAt(t, got, "20260303T090000"); ps != "TENTATIVE" {
		t.Errorf("new override PARTSTAT = %q, want TENTATIVE", ps)
	}
	for name, want := range map[string]string{
		"RECURRENCE-ID": "20260303T090000",
		"DTSTART":       "20260303T090000",
		"DTEND":         "20260303T100000",
	} {
		p := ov.Props.Get(name)
		if p == nil || p.Value != want || p.Params.Get("TZID") != "Europe/Zurich" {
			t.Errorf("%s = %+v, want TZID=Europe/Zurich:%s", name, p, want)
		}
	}
	if ov.Props.Get("RRULE") != nil {
		t.Error("new override kept the RRULE")
	}
	if ps := partStatAt(t, got, ""); ps != "NEEDS-ACTION" {
		t.Errorf("master PARTSTAT = %q, want NEEDS-ACTION", ps)
	}
	if n := countVEVENT(string(EncodeLenient(got))); n != 3 {
		t.Errorf("%d VEVENTs, want 3 (no override for a non-occurrence)", n)
	}
}
