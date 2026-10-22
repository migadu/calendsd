package ical

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// workweek is a single VAVAILABILITY with Mon–Fri 09:00–17:00 in tzid,
// anchored on Monday 2026-01-05; vavail and avail add properties to the
// VAVAILABILITY and the AVAILABLE, extra adds components after it.
func workweek(tzid string, vavail, avail []string, extra ...string) []byte {
	lines := []string{"BEGIN:VAVAILABILITY", "UID:ww", "DTSTAMP:20260101T000000Z"}
	lines = append(lines, vavail...)
	lines = append(lines,
		"BEGIN:AVAILABLE", "UID:ww-days", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID="+tzid+":20260105T090000",
		"DTEND;TZID="+tzid+":20260105T170000",
		"RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR")
	lines = append(lines, avail...)
	lines = append(lines, "END:AVAILABLE")
	lines = append(lines, extra...)
	lines = append(lines, "END:VAVAILABILITY")
	return ics(lines...)
}

// withWEurope embeds an Outlook-style "W. Europe Standard Time" VTIMEZONE
// (CET/CEST) before the first VAVAILABILITY.
func withWEurope(data []byte) []byte {
	return []byte(strings.Replace(string(data), "BEGIN:VAVAILABILITY", strings.Join([]string{
		"BEGIN:VTIMEZONE", "TZID:W. Europe Standard Time",
		"BEGIN:STANDARD", "DTSTART:16011028T030000",
		"TZOFFSETFROM:+0200", "TZOFFSETTO:+0100",
		"RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10", "END:STANDARD",
		"BEGIN:DAYLIGHT", "DTSTART:16010325T020000",
		"TZOFFSETFROM:+0100", "TZOFFSETTO:+0200",
		"RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3", "END:DAYLIGHT",
		"END:VTIMEZONE", "BEGIN:VAVAILABILITY"}, "\r\n"), 1))
}

func utc(s string) time.Time {
	t, err := time.Parse("20060102T150405Z", s)
	if err != nil {
		panic(err)
	}
	return t
}

// unavailable runs unavailablePeriods and renders the result as start/end
// pairs for comparison.
func unavailable(t *testing.T, data []byte, start, end string) []string {
	t.Helper()
	periods, err := unavailablePeriods(data, utc(start), utc(end))
	if err != nil {
		t.Fatalf("unavailablePeriods: %v", err)
	}
	var out []string
	for _, p := range periods {
		if p.fbType != "BUSY-UNAVAILABLE" {
			t.Errorf("period %v has FBTYPE %s", p, p.fbType)
		}
		out = append(out, p.start.UTC().Format("20060102T150405Z")+"/"+p.end.UTC().Format("20060102T150405Z"))
	}
	return out
}

func wantPeriods(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("unavailable periods:\n got: %s\nwant: %s", strings.Join(got, " "), strings.Join(want, " "))
	}
}

// Berlin leaves summer time on Sunday 2026-10-25: a 09–17 day is 07–15Z on
// the Friday before and 08–16Z on the Monday after.
func TestAvailabilityBerlinAcrossDSTEnd(t *testing.T) {
	got := unavailable(t, workweek("Europe/Berlin", nil, nil), "20261023T000000Z", "20261027T000000Z")
	wantPeriods(t, got,
		"20261023T000000Z/20261023T070000Z",
		"20261023T150000Z/20261026T080000Z",
		"20261026T160000Z/20261027T000000Z")
}

// New York leaves summer time a week later, on 2026-11-01, so in the week
// between the two cities are 5 hours apart instead of 6.
func TestAvailabilityNewYorkAcrossDSTEnd(t *testing.T) {
	got := unavailable(t, workweek("America/New_York", nil, nil), "20261030T000000Z", "20261103T000000Z")
	wantPeriods(t, got,
		"20261030T000000Z/20261030T130000Z",
		"20261030T210000Z/20261102T140000Z",
		"20261102T220000Z/20261103T000000Z")

	berlin := unavailable(t, workweek("Europe/Berlin", nil, nil), "20261026T000000Z", "20261027T000000Z")
	wantPeriods(t, berlin, "20261026T000000Z/20261026T080000Z", "20261026T160000Z/20261027T000000Z")
	newYork := unavailable(t, workweek("America/New_York", nil, nil), "20261026T000000Z", "20261027T000000Z")
	wantPeriods(t, newYork, "20261026T000000Z/20261026T130000Z", "20261026T210000Z/20261027T000000Z")
}

// An Outlook-style zone name with an embedded VTIMEZONE follows that zone's
// DST rules, like the IANA zone it stands for.
func TestAvailabilityNonIANATZIDFollowsDST(t *testing.T) {
	data := workweek("W. Europe Standard Time", nil, nil)
	data = withWEurope(data)
	if err := ValidateAvailability(data); err != nil {
		t.Fatalf("ValidateAvailability: %v", err)
	}
	got := unavailable(t, data, "20261023T000000Z", "20261027T000000Z")
	wantPeriods(t, got,
		"20261023T000000Z/20261023T070000Z",
		"20261023T150000Z/20261026T080000Z",
		"20261026T160000Z/20261027T000000Z")
}

// A start inside the spring-forward gap lands where Go (and so
// recurrenceSet) places gap times; pinned so a change is noticed.
func TestAvailabilitySpringForwardGap(t *testing.T) {
	data := ics("BEGIN:VAVAILABILITY", "UID:gap", "DTSTAMP:20260101T000000Z",
		"BEGIN:AVAILABLE", "UID:gap-1", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=Europe/Berlin:20260322T023000",
		"DTEND;TZID=Europe/Berlin:20260322T043000",
		"RRULE:FREQ=WEEKLY", "END:AVAILABLE", "END:VAVAILABILITY")
	berlin, _ := time.LoadLocation("Europe/Berlin")
	gapStart := time.Date(2026, 3, 29, 2, 30, 0, 0, berlin).UTC()
	periods, err := unavailablePeriods(data, utc("20260329T000000Z"), utc("20260330T000000Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 2 || !periods[0].end.Equal(gapStart) {
		t.Fatalf("periods = %+v, want the available span to start at %s", periods, gapStart)
	}
}

// CalDAVTester availability 1.ics shape: a bounded VAVAILABILITY whose
// Friday instance is overridden to 12–17.
func TestAvailabilityBoundedWithOverride(t *testing.T) {
	data := workweek("Europe/Berlin",
		[]string{"DTSTART;TZID=Europe/Berlin:20260601T000000", "DTEND;TZID=Europe/Berlin:20260615T000000"},
		nil,
		"BEGIN:AVAILABLE", "UID:ww-days", "DTSTAMP:20260101T000000Z",
		"RECURRENCE-ID;TZID=Europe/Berlin:20260605T090000",
		"DTSTART;TZID=Europe/Berlin:20260605T120000",
		"DTEND;TZID=Europe/Berlin:20260605T170000",
		"END:AVAILABLE")
	if err := ValidateAvailability(data); err != nil {
		t.Fatalf("ValidateAvailability: %v", err)
	}
	// Thursday 4th normal, Friday 5th from 12:00 (10:00Z).
	got := unavailable(t, data, "20260604T000000Z", "20260606T000000Z")
	wantPeriods(t, got,
		"20260604T000000Z/20260604T070000Z",
		"20260604T150000Z/20260605T100000Z",
		"20260605T150000Z/20260606T000000Z")
	// Outside the coverage nothing is unavailable.
	wantPeriods(t, unavailable(t, data, "20260701T000000Z", "20260702T000000Z"))
	// The coverage ends at local midnight on the 15th (22:00Z on the 14th).
	got = unavailable(t, data, "20260614T000000Z", "20260616T000000Z")
	wantPeriods(t, got, "20260614T000000Z/20260614T220000Z")
}

func TestAvailabilityExdateRdateAndWindowEdges(t *testing.T) {
	data := workweek("Europe/Berlin", nil, []string{
		"EXDATE;TZID=Europe/Berlin:20260602T090000,20260603T090000",
		"RDATE;TZID=Europe/Berlin:20260606T090000",
	})
	got := unavailable(t, data, "20260601T000000Z", "20260608T000000Z")
	wantPeriods(t, got,
		"20260601T000000Z/20260601T070000Z",
		"20260601T150000Z/20260604T070000Z", // Tue and Wed excluded
		"20260604T150000Z/20260605T070000Z",
		"20260605T150000Z/20260606T070000Z", // Saturday added by RDATE
		"20260606T150000Z/20260608T000000Z")
	// A window entirely inside working hours has nothing unavailable.
	wantPeriods(t, unavailable(t, data, "20260601T080000Z", "20260601T140000Z"))
	// A weekly rule anchored long ago still reaches a window far in the
	// future (no maxRecurrences cut-off).
	got = unavailable(t, workweek("Europe/Berlin", nil, nil), "20500103T000000Z", "20500104T000000Z")
	wantPeriods(t, got, "20500103T000000Z/20500103T080000Z", "20500103T160000Z/20500104T000000Z")
}

// Two VAVAILABILITY components of equal priority combine: time unavailable
// in either is unavailable.
func TestAvailabilityMergesComponents(t *testing.T) {
	data := ics(
		"BEGIN:VAVAILABILITY", "UID:a", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260601T000000Z", "DTEND:20260602T000000Z",
		"BEGIN:AVAILABLE", "UID:a1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260601T080000Z", "DTEND:20260601T120000Z", "END:AVAILABLE",
		"END:VAVAILABILITY",
		"BEGIN:VAVAILABILITY", "UID:b", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260601T120000Z", "DTEND:20260603T000000Z",
		"BEGIN:AVAILABLE", "UID:b1", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260602T080000Z", "DURATION:PT4H", "END:AVAILABLE",
		"END:VAVAILABILITY")
	got := unavailable(t, data, "20260531T000000Z", "20260604T000000Z")
	wantPeriods(t, got,
		"20260601T000000Z/20260601T080000Z",
		"20260601T120000Z/20260602T080000Z",
		"20260602T120000Z/20260603T000000Z")
}

func TestValidateAvailabilityRejects(t *testing.T) {
	valid := string(workweek("Europe/Berlin", nil, nil))
	cases := map[string]string{
		"not a calendar": "This is not a calendar!",
		"event": string(ics("BEGIN:VEVENT", "UID:e", "DTSTAMP:20260101T000000Z",
			"DTSTART:20260101T090000Z", "END:VEVENT")),
		"no vavailability": string(ics()),
		"floating":         strings.Replace(valid, "DTSTART;TZID=Europe/Berlin:", "DTSTART:", 1),
		"date":             strings.Replace(valid, "DTSTART;TZID=Europe/Berlin:20260105T090000", "DTSTART;VALUE=DATE:20260105", 1),
		"unknown tzid":     strings.ReplaceAll(valid, "Europe/Berlin", "Mars/Olympus"),
		"no uid":           strings.Replace(valid, "UID:ww-days\r\n", "", 1),
		"no dtstamp":       strings.Replace(valid, "UID:ww\r\nDTSTAMP:20260101T000000Z\r\n", "UID:ww\r\n", 1),
		"no end":           strings.Replace(valid, "DTEND;TZID=Europe/Berlin:20260105T170000\r\n", "", 1),
		"end before start": strings.Replace(valid, "20260105T170000", "20260105T080000", 1),
		"minutely":         strings.Replace(valid, "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", "FREQ=MINUTELY", 1),
		"hourly":           strings.Replace(valid, "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", "FREQ=HOURLY", 1),
		"byhour":           strings.Replace(valid, "BYDAY=MO,TU,WE,TH,FR", "BYDAY=MO,TU,WE,TH,FR;BYHOUR=9,13", 1),
		"byminute":         strings.Replace(valid, "BYDAY=MO,TU,WE,TH,FR", "BYDAY=MO,TU,WE,TH,FR;BYMINUTE=0,30", 1),
		"bysecond":         strings.Replace(valid, "BYDAY=MO,TU,WE,TH,FR", "BYDAY=MO,TU,WE,TH,FR;BYSECOND=0,1", 1),
		// Daily since 1700 passes the scan cap within availabilityHorizon.
		"anchored too far back": strings.Replace(strings.Replace(valid,
			"FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", "FREQ=DAILY", 1), "20260105T", "17000105T", 2),
		"priority out of range":    strings.Replace(valid, "UID:ww\r\n", "UID:ww\r\nPRIORITY:10\r\n", 1),
		"priority not a number":    strings.Replace(valid, "UID:ww\r\n", "UID:ww\r\nPRIORITY:high\r\n", 1),
		"duration without dtstart": strings.Replace(valid, "UID:ww\r\n", "UID:ww\r\nDURATION:P1W\r\n", 1),
		"duration and dtend": strings.Replace(valid, "UID:ww\r\n",
			"UID:ww\r\nDTSTART:20260101T000000Z\r\nDTEND:20260201T000000Z\r\nDURATION:P1W\r\n", 1),
		"negative duration": strings.Replace(valid, "UID:ww\r\n",
			"UID:ww\r\nDTSTART:20260101T000000Z\r\nDURATION:-P1W\r\n", 1),
		"duplicate master": strings.Replace(valid, "END:VAVAILABILITY",
			"BEGIN:AVAILABLE\r\nUID:ww-days\r\nDTSTAMP:20260101T000000Z\r\n"+
				"DTSTART;TZID=Europe/Berlin:20260110T090000\r\nDTEND;TZID=Europe/Berlin:20260110T120000\r\n"+
				"END:AVAILABLE\r\nEND:VAVAILABILITY", 1),
		"oversized": valid + strings.Repeat("X", MaxAvailabilitySize),
	}
	if err := ValidateAvailability([]byte(valid)); err != nil {
		t.Fatalf("valid value rejected: %v", err)
	}
	for name, data := range cases {
		err := ValidateAvailability([]byte(data))
		if err == nil {
			t.Errorf("%s: accepted, want rejection", name)
		} else if !errors.Is(err, ErrInvalidAvailability) {
			t.Errorf("%s: error %v does not wrap ErrInvalidAvailability", name, err)
		}
	}
}

// GenerateFreeBusy is unchanged by availability (the sabre golden test pins
// it); the availability variant adds BUSY-UNAVAILABLE next to BUSY, and an
// event outside working hours is still listed as BUSY.
func TestGenerateFreeBusyWithAvailability(t *testing.T) {
	event := ics("BEGIN:VEVENT", "UID:late", "DTSTAMP:20260101T000000Z",
		"DTSTART:20260601T180000Z", "DTEND:20260601T190000Z", "END:VEVENT")
	start, end := utc("20260601T000000Z"), utc("20260602T000000Z")
	got, err := GenerateFreeBusyWithAvailability([][]byte{event}, workweek("Europe/Berlin", nil, nil),
		start, end, time.UTC, start)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DTSTART:20260601T000000Z",
		"DTEND:20260602T000000Z",
		"FREEBUSY;FBTYPE=BUSY-UNAVAILABLE:20260601T000000Z/20260601T070000Z",
		"FREEBUSY;FBTYPE=BUSY-UNAVAILABLE:20260601T150000Z/20260602T000000Z",
		"FREEBUSY:20260601T180000Z/20260601T190000Z",
	}
	var lines []string
	for _, l := range fbLines(string(got)) {
		if strings.HasPrefix(l, "DTSTART") || strings.HasPrefix(l, "DTEND") || strings.HasPrefix(l, "FREEBUSY") {
			lines = append(lines, l)
		}
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("free-busy:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	_, err = GenerateFreeBusyWithAvailability(nil, []byte("garbage"), start, end, time.UTC, start)
	if !errors.Is(err, ErrInvalidAvailability) {
		t.Errorf("garbage availability: err = %v, want ErrInvalidAvailability", err)
	}
}

// A stored rule that cannot reach the window within the scan cap (one that
// bypassed validation) is reported unusable, not as "unavailable all day".
func TestAvailabilityScanCapIsAnError(t *testing.T) {
	data := ics("BEGIN:VAVAILABILITY", "UID:h", "DTSTAMP:20260101T000000Z",
		"BEGIN:AVAILABLE", "UID:h1", "DTSTAMP:20260101T000000Z",
		"DTSTART:19000101T000000Z", "DTEND:19000101T003000Z",
		"RRULE:FREQ=HOURLY", "END:AVAILABLE", "END:VAVAILABILITY")
	_, err := unavailablePeriods(data, utc("20260601T000000Z"), utc("20260602T000000Z"))
	if !errors.Is(err, ErrInvalidAvailability) {
		t.Fatalf("err = %v, want ErrInvalidAvailability", err)
	}
}

// An Outlook-zone series with a UTC UNTIL ends on the right instance: UNTIL
// 07:00Z is 09:00 local in summer, so Wednesday 2026-06-03's 09:00 instance is
// the last one, and Thursday is unavailable all day.
func TestAvailabilityNonIANAUntil(t *testing.T) {
	data := workweek("W. Europe Standard Time", nil, nil)
	data = []byte(strings.Replace(string(data), "BYDAY=MO,TU,WE,TH,FR", "BYDAY=MO,TU,WE,TH,FR;UNTIL=20260603T070000Z", 1))
	data = withWEurope(data)
	got := unavailable(t, data, "20260603T000000Z", "20260605T000000Z")
	wantPeriods(t, got,
		"20260603T000000Z/20260603T070000Z",
		"20260603T150000Z/20260605T000000Z")
}

// A higher-PRIORITY VAVAILABILITY decides all the time it covers: a week of
// 12–20 layered over regular 09–17 (Berlin, CEST) is free 10:00Z–18:00Z on
// its days, and the regular hours still apply outside that week. PRIORITY 0
// ranks below 9.
func TestAvailabilityPriorityOverrides(t *testing.T) {
	data := ics(
		"BEGIN:VAVAILABILITY", "UID:base", "DTSTAMP:20260101T000000Z", "PRIORITY:0",
		"BEGIN:AVAILABLE", "UID:base-days", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=Europe/Berlin:20260105T090000", "DTEND;TZID=Europe/Berlin:20260105T170000",
		"RRULE:FREQ=DAILY", "END:AVAILABLE", "END:VAVAILABILITY",
		"BEGIN:VAVAILABILITY", "UID:late", "DTSTAMP:20260101T000000Z", "PRIORITY:9",
		"DTSTART;TZID=Europe/Berlin:20260601T000000", "DTEND;TZID=Europe/Berlin:20260608T000000",
		"BEGIN:AVAILABLE", "UID:late-days", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=Europe/Berlin:20260601T120000", "DTEND;TZID=Europe/Berlin:20260601T200000",
		"RRULE:FREQ=DAILY", "END:AVAILABLE", "END:VAVAILABILITY")
	if err := ValidateAvailability(data); err != nil {
		t.Fatalf("ValidateAvailability: %v", err)
	}
	got := unavailable(t, data, "20260607T000000Z", "20260609T000000Z")
	wantPeriods(t, got,
		"20260607T000000Z/20260607T100000Z",
		"20260607T180000Z/20260608T070000Z",
		"20260608T150000Z/20260609T000000Z")
}

// On an Outlook-zone series, EXDATE and RDATE written in UTC name the
// instants they say: Wednesday 2026-06-03 09:00 CEST (07:00Z) is excluded,
// and an extra Saturday 10:00 CEST (08:00Z) for two hours is added.
func TestAvailabilityNonIANAUTCExdateRdate(t *testing.T) {
	data := withWEurope(workweek("W. Europe Standard Time", nil, []string{
		"EXDATE:20260603T070000Z", "RDATE:20260606T080000Z"}))
	if err := ValidateAvailability(data); err != nil {
		t.Fatalf("ValidateAvailability: %v", err)
	}
	got := unavailable(t, data, "20260603T000000Z", "20260604T000000Z")
	wantPeriods(t, got, "20260603T000000Z/20260604T000000Z")
	got = unavailable(t, data, "20260606T000000Z", "20260607T000000Z")
	wantPeriods(t, got,
		"20260606T000000Z/20260606T080000Z",
		"20260606T160000Z/20260607T000000Z")
}

// A DTEND in another zone than an Outlook-zone DTSTART still gives the
// instance its real length: 09:00 CEST to 15:00Z is 09–17 local.
func TestAvailabilityNonIANAMixedZoneEnd(t *testing.T) {
	data := withWEurope(workweek("W. Europe Standard Time", nil, nil))
	data = []byte(strings.Replace(string(data),
		"DTEND;TZID=W. Europe Standard Time:20260105T170000", "DTEND:20260105T160000Z", 1))
	if err := ValidateAvailability(data); err != nil {
		t.Fatalf("ValidateAvailability: %v", err)
	}
	got := unavailable(t, data, "20260603T000000Z", "20260604T000000Z")
	wantPeriods(t, got,
		"20260603T000000Z/20260603T070000Z",
		"20260603T150000Z/20260604T000000Z")
}

// A floating EXDATE on an Outlook-zone series (only possible in an imported
// value; validation rejects it) is read on the zone's wall clock, as before
// UTC values were moved onto it.
func TestAvailabilityNonIANAFloatingExdate(t *testing.T) {
	data := withWEurope(workweek("W. Europe Standard Time", nil, []string{"EXDATE:20260603T090000"}))
	got := unavailable(t, data, "20260603T000000Z", "20260604T000000Z")
	wantPeriods(t, got, "20260603T000000Z/20260604T000000Z")
}

// CalDAVTester's availability.xml "free-busy reports/1" (availability/17.xml,
// 4.ics, 19.ics, with $now.year.0 = 2026 and $now.year.1 = 2027): Montreal
// weekday hours and a one-hour US/Eastern event. CalendarServer expects
// BUSY 17:00Z/PT1H and BUSY-UNAVAILABLE 00:00Z–14:00Z and 22:00Z–24:00Z on
// one line; calendsd writes the same periods in sabre's format, which is
// why the suite itself still fails this test (scripts/caldavtester/README.md).
func TestGenerateFreeBusyCalDAVTesterAvailability(t *testing.T) {
	montreal := []string{
		"BEGIN:VTIMEZONE", "TZID:America/Montreal",
		"BEGIN:DAYLIGHT", "DTSTART:20000404T020000", "RRULE:FREQ=YEARLY;BYDAY=1SU;BYMONTH=4",
		"TZOFFSETFROM:-0500", "TZOFFSETTO:-0400", "END:DAYLIGHT",
		"BEGIN:STANDARD", "DTSTART:20001026T020000", "RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10",
		"TZOFFSETFROM:-0400", "TZOFFSETTO:-0500", "END:STANDARD",
		"END:VTIMEZONE",
	}
	availability := ics(append(montreal,
		"BEGIN:VAVAILABILITY", "ORGANIZER:mailto:bernard@example.com",
		"UID:20061005T133225Z-00002-availability@example.com", "DTSTAMP:20061005T133225Z",
		"DTSTART;TZID=America/Montreal:20260101T000000",
		"BEGIN:AVAILABLE", "UID:20061005T133225Z-00001-A-availability@example.com",
		"DTSTAMP:20061005T133225Z",
		"DTSTART;TZID=America/Montreal:20260105T090000",
		"DTEND;TZID=America/Montreal:20260105T170000",
		"RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR",
		"END:AVAILABLE", "END:VAVAILABILITY")...)
	event := ics("BEGIN:VEVENT", "UID:event1", "DTSTAMP:20051222T205953Z",
		"DTSTART;TZID=US/Eastern:20270104T120000", "DURATION:PT1H", "END:VEVENT")
	if err := ValidateAvailability(availability); err != nil {
		t.Fatalf("ValidateAvailability: %v", err)
	}
	start, end := utc("20270104T000000Z"), utc("20270105T000000Z")
	got, err := GenerateFreeBusyWithAvailability([][]byte{event}, availability, start, end, time.UTC, start)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range fbLines(string(got)) {
		if strings.HasPrefix(l, "FREEBUSY") {
			lines = append(lines, l)
		}
	}
	want := []string{
		"FREEBUSY;FBTYPE=BUSY-UNAVAILABLE:20270104T000000Z/20270104T140000Z",
		"FREEBUSY:20270104T170000Z/20270104T180000Z",
		"FREEBUSY;FBTYPE=BUSY-UNAVAILABLE:20270104T220000Z/20270105T000000Z",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("free-busy:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
