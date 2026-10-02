package ical

// bugs_test.go pins down confirmed bugs in the iCalendar library. Each test
// asserts the CORRECT (spec-compliant) behavior, so it FAILS against the
// current buggy code — that is intentional. Once a bug is fixed the matching
// test should turn green.
//
// Helpers ics/event (denormalize_test.go) and wrapCal/body/countVEVENT/findMsg
// (itip_instances_test.go) are reused from the package's other test files.

import (
	"strings"
	"testing"
	"time"

	goical "github.com/emersion/go-ical"
)

// eventByRID returns the first VEVENT in cal whose RECURRENCE-ID value equals
// rid ("" for the master, i.e. no RECURRENCE-ID prop).
func eventByRID(cal *goical.Calendar, rid string) *goical.Component {
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		got := ""
		if p := comp.Props.Get("RECURRENCE-ID"); p != nil {
			got = p.Value
		}
		if got == rid {
			return comp
		}
	}
	return nil
}

// attendeePartStat returns the PARTSTAT of the ATTENDEE whose value contains
// the given address substring, in comp.
func attendeePartStat(comp *goical.Component, addr string) (string, bool) {
	for _, att := range comp.Props.Values("ATTENDEE") {
		if strings.Contains(strings.ToLower(att.Value), strings.ToLower(addr)) {
			return att.Params.Get("PARTSTAT"), true
		}
	}
	return "", false
}

// TestBug1AttendeeDeleteRepliesDeclined covers itip.go:414-534.
//
// When an attendee deletes their copy of the event, ParseEvent(nil, addrs,
// oldCal) must emit a REPLY declining on the attendee's behalf: PARTSTAT=
// DECLINED and a valid DTSTART matching the original event's DTSTART.
//
// Expected: ATTENDEE PARTSTAT=DECLINED, DTSTART:20260301T090000Z.
// Actual (buggy): newInfo is nil, so in.newStatus is "" (empty PARTSTAT) and
// instanceOf(newInfo, "master") is nil, so DTSTART is synthesized from the id
// literal -> "DTSTART:master".
func TestBug1AttendeeDeleteRepliesDeclined(t *testing.T) {
	oldCal, err := Decode([]byte(wrapCal([]string{
		"BEGIN:VEVENT", "UID:del-1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260301T090000Z", "DTEND:20260301T100000Z",
		"SUMMARY:Lunch",
		"ORGANIZER;CN=Org:mailto:org@example.org",
		"ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org",
		"END:VEVENT",
	})))
	if err != nil {
		t.Fatal(err)
	}

	msgs, err := ParseEvent(nil, []string{"mailto:a@example.org"}, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Method != "REPLY" {
		t.Fatalf("want one REPLY, got %+v", msgs)
	}

	rb := body(t, msgs[0])
	if !strings.Contains(rb, "PARTSTAT=DECLINED") {
		t.Errorf("attendee delete must reply DECLINED, got:\n%s", rb)
	}
	if strings.Contains(rb, "DTSTART;VALUE=DATE:master") || strings.Contains(rb, "DTSTART:master") {
		t.Errorf("reply carries a bogus 'master' literal DTSTART:\n%s", rb)
	}
	if !strings.Contains(rb, "DTSTART:20260301T090000Z") {
		t.Errorf("reply DTSTART must match the original event's DTSTART:\n%s", rb)
	}
}

// TestBug2ProcessReplyPerInstancePartStat covers itip.go:697-726.
//
// ProcessMessage(REPLY) must apply each attendee PARTSTAT to the VEVENT that
// matches by RECURRENCE-ID, not stamp the first-seen attendee's PARTSTAT onto
// every VEVENT. Here the reply ACCEPTs the master but DECLINEs the override.
//
// Expected: existing master ATTENDEE PARTSTAT=ACCEPTED, override ATTENDEE
// PARTSTAT=DECLINED.
// Actual (buggy): the loop reads replyInfo.attendees[0] (first-seen only) and
// stamps that PARTSTAT onto all VEVENTs, so both come out identical.
func TestBug2ProcessReplyPerInstancePartStat(t *testing.T) {
	// Organizer's stored copy: recurring master + one override, attendee A
	// still NEEDS-ACTION on both.
	existing, err := Decode([]byte(wrapCal(
		[]string{
			"BEGIN:VEVENT", "UID:rep-1", "DTSTAMP:20260101T000000Z",
			"DTSTART:20260101T100000Z", "DURATION:PT1H", "RRULE:FREQ=DAILY;COUNT=3",
			"SUMMARY:Series",
			"ORGANIZER;CN=Org:mailto:org@example.org",
			"ATTENDEE;CN=A;PARTSTAT=NEEDS-ACTION:mailto:a@example.org",
			"END:VEVENT",
		},
		[]string{
			"BEGIN:VEVENT", "UID:rep-1", "DTSTAMP:20260101T000000Z",
			"RECURRENCE-ID:20260102T100000Z", "DTSTART:20260102T100000Z", "DURATION:PT1H",
			"SUMMARY:Series (day 2)",
			"ORGANIZER;CN=Org:mailto:org@example.org",
			"ATTENDEE;CN=A;PARTSTAT=NEEDS-ACTION:mailto:a@example.org",
			"END:VEVENT",
		},
	)))
	if err != nil {
		t.Fatal(err)
	}

	// Synthesize the reply via ParseEvent on the attendee side: A accepts the
	// master but declines the day-2 override.
	oldCal, err := Decode([]byte(wrapCal(
		[]string{
			"BEGIN:VEVENT", "UID:rep-1", "DTSTAMP:20260101T000000Z",
			"DTSTART:20260101T100000Z", "DURATION:PT1H", "RRULE:FREQ=DAILY;COUNT=3",
			"ORGANIZER;CN=Org:mailto:org@example.org",
			"ATTENDEE;CN=A;PARTSTAT=NEEDS-ACTION:mailto:a@example.org",
			"END:VEVENT",
		},
		[]string{
			"BEGIN:VEVENT", "UID:rep-1", "DTSTAMP:20260101T000000Z",
			"RECURRENCE-ID:20260102T100000Z", "DTSTART:20260102T100000Z", "DURATION:PT1H",
			"ORGANIZER;CN=Org:mailto:org@example.org",
			"ATTENDEE;CN=A;PARTSTAT=NEEDS-ACTION:mailto:a@example.org",
			"END:VEVENT",
		},
	)))
	if err != nil {
		t.Fatal(err)
	}
	newCal, err := Decode([]byte(wrapCal(
		[]string{
			"BEGIN:VEVENT", "UID:rep-1", "DTSTAMP:20260101T000000Z",
			"DTSTART:20260101T100000Z", "DURATION:PT1H", "RRULE:FREQ=DAILY;COUNT=3",
			"ORGANIZER;CN=Org:mailto:org@example.org",
			"ATTENDEE;CN=A;PARTSTAT=ACCEPTED:mailto:a@example.org",
			"END:VEVENT",
		},
		[]string{
			"BEGIN:VEVENT", "UID:rep-1", "DTSTAMP:20260101T000000Z",
			"RECURRENCE-ID:20260102T100000Z", "DTSTART:20260102T100000Z", "DURATION:PT1H",
			"ORGANIZER;CN=Org:mailto:org@example.org",
			"ATTENDEE;CN=A;PARTSTAT=DECLINED:mailto:a@example.org",
			"END:VEVENT",
		},
	)))
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ParseEvent(newCal, []string{"mailto:a@example.org"}, oldCal)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Method != "REPLY" {
		t.Fatalf("want one REPLY from the attendee change, got %+v", msgs)
	}

	updated, err := ProcessMessage(msgs[0], existing)
	if err != nil {
		t.Fatal(err)
	}
	if updated == nil {
		t.Fatal("ProcessMessage returned nil")
	}

	master := eventByRID(updated, "")
	override := eventByRID(updated, "20260102T100000Z")
	if master == nil || override == nil {
		t.Fatalf("missing master/override after apply: master=%v override=%v", master, override)
	}
	mps, _ := attendeePartStat(master, "a@example.org")
	ops, _ := attendeePartStat(override, "a@example.org")
	if mps != "ACCEPTED" {
		t.Errorf("master ATTENDEE PARTSTAT = %q, want ACCEPTED", mps)
	}
	if ops != "DECLINED" {
		t.Errorf("override ATTENDEE PARTSTAT = %q, want DECLINED", ops)
	}
	if mps == ops {
		t.Errorf("per-instance PARTSTAT was not applied: both = %q "+
			"(reply stamped first-seen attendee onto all VEVENTs)", mps)
	}
}

// TestBug3ExpandOverrideMovedIntoWindow covers expand.go:141-159.
//
// An overridden instance whose DTSTART was moved INTO the query window must be
// included even though its original recurrence slot (RECURRENCE-ID) lies
// outside the window.
//
// Fixture: weekly event starting 2026-08-20T10:00Z with an override
// (RECURRENCE-ID=2026-08-20T10:00Z) rescheduled to DTSTART=2026-08-05T10:00Z.
// Expand over [2026-08-01, 2026-08-10).
//
// Expected: expansion contains an instance at 2026-08-05T10:00Z.
// Actual (buggy): the RRULE iterator starts at 2026-08-20 (past window end),
// so the loop breaks before reaching the override slot and the moved instance
// is dropped.
func TestBug3ExpandOverrideMovedIntoWindow(t *testing.T) {
	cal, err := Decode(ics(
		"BEGIN:VEVENT", "UID:mov-1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260820T100000Z", "DTEND:20260820T110000Z",
		"RRULE:FREQ=WEEKLY", "SUMMARY:Weekly", "END:VEVENT",
		"BEGIN:VEVENT", "UID:mov-1", "DTSTAMP:20260101T000000Z",
		"RECURRENCE-ID:20260820T100000Z",
		"DTSTART:20260805T100000Z", "DTEND:20260805T110000Z",
		"SUMMARY:Weekly (moved earlier)", "END:VEVENT",
	))
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := Expand(cal,
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		time.UTC)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	found := false
	for _, comp := range expanded.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		if p := comp.Props.Get("DTSTART"); p != nil && p.Value == "20260805T100000Z" {
			found = true
		}
	}
	if !found {
		t.Errorf("expansion must include the override moved into the window "+
			"(DTSTART:20260805T100000Z), instances:\n%s", body(t, &Message{Calendar: expanded}))
	}
}

// TestBug4ExpandAllDayKeepsDateValue covers expand.go:164-168.
//
// A recurring all-day event must keep VALUE=DATE and the correct calendar
// date after expansion, not be shifted to a timezone-adjusted UTC date-time.
//
// Fixture: DTSTART;VALUE=DATE:20260801, DAILY COUNT=3, expanded with
// loc=Europe/Berlin.
//
// Expected: first instance DTSTART is a DATE value 20260801.
// Actual (buggy): setUTCDateTime formats occStart (midnight Berlin = the day
// before at 22:00Z) as a UTC date-time -> "20260731T220000Z".
func TestBug4ExpandAllDayKeepsDateValue(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	cal, err := Decode(ics(
		"BEGIN:VEVENT", "UID:allday-1", "DTSTAMP:20260101T000000Z",
		"DTSTART;VALUE=DATE:20260801", "RRULE:FREQ=DAILY;COUNT=3",
		"SUMMARY:All day series", "END:VEVENT",
	))
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := Expand(cal,
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC),
		berlin)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	var first *goical.Component
	for _, comp := range expanded.Children {
		if comp.Name == goical.CompEvent {
			first = comp
			break
		}
	}
	if first == nil {
		t.Fatal("no expanded instance produced")
	}
	ds := first.Props.Get("DTSTART")
	if ds == nil {
		t.Fatal("expanded instance missing DTSTART")
	}
	if ds.Value != "20260801" {
		t.Errorf("all-day DTSTART = %q, want DATE value %q", ds.Value, "20260801")
	}
	if ds.Params.Get("VALUE") != "DATE" {
		t.Errorf("all-day DTSTART lost VALUE=DATE (got VALUE=%q, value %q)",
			ds.Params.Get("VALUE"), ds.Value)
	}
}

// TestBug5MultiValueExdate covers expand.go:134 / timerange.go:34 /
// denormalize.go:154.
//
// A valid event with a comma-separated multi-value EXDATE must be handled
// without error, and the excluded instances must actually be excluded from
// expansion. go-ical's RecurrenceSet chokes on the multi-value form.
//
// Fixture: weekly event with EXDATE:20260101T100000Z,20260108T100000Z (two
// occurrences excluded).
//
// Expected: Denormalize + Expand succeed with no error, and the Jan 1 and
// Jan 8 occurrences are absent from the expansion (Jan 15 remains).
func TestBug5MultiValueExdate(t *testing.T) {
	data := ics(
		"BEGIN:VEVENT", "UID:exd-1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260101T100000Z", "DTEND:20260101T110000Z",
		"RRULE:FREQ=WEEKLY;COUNT=3",
		"EXDATE:20260101T100000Z,20260108T100000Z",
		"SUMMARY:Weekly with exdates", "END:VEVENT",
	)

	if _, err := Denormalize(data); err != nil {
		t.Fatalf("Denormalize of multi-value EXDATE errored: %v", err)
	}

	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := Expand(cal,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		time.UTC)
	if err != nil {
		t.Fatalf("Expand of multi-value EXDATE errored: %v", err)
	}

	var starts []string
	for _, comp := range expanded.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		if p := comp.Props.Get("DTSTART"); p != nil {
			starts = append(starts, p.Value)
		}
	}
	for _, excluded := range []string{"20260101T100000Z", "20260108T100000Z"} {
		for _, s := range starts {
			if s == excluded {
				t.Errorf("EXDATE %s was not excluded from expansion; instances: %v", excluded, starts)
			}
		}
	}
	found := false
	for _, s := range starts {
		if s == "20260115T100000Z" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the surviving Jan 15 occurrence; instances: %v", starts)
	}
}

// TestBug6NonIANATZIDWithVTimezone covers the VTIMEZONE / non-IANA TZID case
// in expand.go.
//
// An event whose DTSTART uses a non-IANA TZID (e.g. "W. Europe Standard Time",
// as Outlook emits) with an embedded VTIMEZONE definition should Denormalize
// and Expand without error, resolving the offset from the VTIMEZONE.
//
// If go-ical cannot resolve non-IANA TZIDs from an embedded VTIMEZONE (a
// dependency limitation), this test documents the gap rather than forcing a
// pass.
func TestBug6NonIANATZIDWithVTimezone(t *testing.T) {
	data := ics(
		"BEGIN:VTIMEZONE", "TZID:W. Europe Standard Time",
		"BEGIN:STANDARD", "DTSTART:16011028T030000",
		"TZOFFSETFROM:+0200", "TZOFFSETTO:+0100",
		"RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10", "END:STANDARD",
		"BEGIN:DAYLIGHT", "DTSTART:16010325T020000",
		"TZOFFSETFROM:+0100", "TZOFFSETTO:+0200",
		"RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3", "END:DAYLIGHT",
		"END:VTIMEZONE",
		"BEGIN:VEVENT", "UID:tz-nia-1", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=W. Europe Standard Time:20260601T100000",
		"DTEND;TZID=W. Europe Standard Time:20260601T110000",
		"SUMMARY:Outlook meeting", "END:VEVENT",
	)

	if _, err := Denormalize(data); err != nil {
		t.Fatalf("Denormalize of non-IANA TZID with VTIMEZONE errored: %v", err)
	}
	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Expand(cal,
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		time.UTC); err != nil {
		t.Fatalf("Expand of non-IANA TZID with VTIMEZONE errored: %v", err)
	}
}

// TestNonIANATZIDSummerOffset: an Outlook VTIMEZONE anchors its rules in
// 1601, and rrule-go ends a series ~292 years after DTSTART, so the
// transitions used to stop in 1893 and every later summer date was read with
// the winter offset. 10:00 W. Europe in June is 08:00Z, not 09:00Z.
func TestNonIANATZIDSummerOffset(t *testing.T) {
	data := ics(
		"BEGIN:VTIMEZONE", "TZID:W. Europe Standard Time",
		"BEGIN:STANDARD", "DTSTART:16011028T030000",
		"TZOFFSETFROM:+0200", "TZOFFSETTO:+0100",
		"RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10", "END:STANDARD",
		"BEGIN:DAYLIGHT", "DTSTART:16010325T020000",
		"TZOFFSETFROM:+0100", "TZOFFSETTO:+0200",
		"RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3", "END:DAYLIGHT",
		"END:VTIMEZONE",
		"BEGIN:VEVENT", "UID:tz-nia-2", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=W. Europe Standard Time:20260601T100000",
		"DTEND;TZID=W. Europe Standard Time:20260601T110000",
		"END:VEVENT",
	)
	cal, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Expand(cal,
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Children) != 1 {
		t.Fatalf("expanded %d components, want 1", len(out.Children))
	}
	if got := out.Children[0].Props.Get("DTSTART").Value; got != "20260601T080000Z" {
		t.Errorf("DTSTART = %s, want 20260601T080000Z", got)
	}
}
