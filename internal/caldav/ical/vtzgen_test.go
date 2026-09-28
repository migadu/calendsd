package ical

// docs/dst-recurring-events.md §5: generated VTIMEZONEs, checked with the
// independent reader (parseVTimezone, which works from the component's own
// observances and never looks its TZID up) against Go's zone data — daily and
// on both sides of every transition — plus the expansion regression the plan
// starts from.

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	goical "github.com/emersion/go-ical"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("zone %s not available: %v", name, err)
	}
	return loc
}

// checkAgainstLocation compares a generated definition with loc over
// [from, to]: every day at noon UTC, and one second either side of each
// transition.
func checkAgainstLocation(t *testing.T, comp *goical.Component, loc *time.Location, from, to time.Time) {
	t.Helper()
	z := parseVTimezone(comp)
	if z == nil {
		t.Fatal("definition has no observances")
	}
	cmp := func(at time.Time) {
		_, want := at.In(loc).Zone()
		if got := z.offsetAtInstant(at); got != want {
			t.Fatalf("%s at %s: definition %s, zone data %s", loc, at.Format(time.RFC3339),
				formatUTCOffset(got), formatUTCOffset(want))
		}
	}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		cmp(time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, time.UTC))
	}
	for _, tr := range zoneTransitions(loc, from, to) {
		cmp(tr.at.Add(-time.Second))
		cmp(tr.at)
	}
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestExpandZonedSeriesKeepsLocalTime(t *testing.T) {
	zrh := mustLoc(t, "Europe/Zurich")
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART;TZID=Europe/Zurich:20260313T090000\r\nDTEND;TZID=Europe/Zurich:20260313T100000\r\n" +
		"RRULE:FREQ=WEEKLY\r\nSUMMARY:s\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	// Across both transitions of 2026 (29 March, 25 October).
	out, err := Expand(cal, day(2026, 3, 1), day(2026, 12, 1), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Children) < 30 {
		t.Fatalf("expanded %d occurrences", len(out.Children))
	}
	for _, c := range out.Children {
		ts, err := c.Props.Get("DTSTART").DateTime(time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if l := ts.In(zrh); l.Hour() != 9 || l.Minute() != 0 {
			t.Fatalf("occurrence at %s local, want 09:00", l)
		}
	}
}

func TestGenerateVTimezoneMatchesZoneData(t *testing.T) {
	cases := []struct {
		zone string
		cov  Coverage
		to   time.Time
	}{
		// A DST zone, a series running for decades, unbounded into the future.
		{"Europe/Zurich", Coverage{Start: day(2026, 3, 13), Unbounded: true}, day(2099, 12, 1)},
		// Longer than 20 years, bounded.
		{"Europe/Zurich", Coverage{Start: day(1990, 1, 1), End: day(2060, 1, 1)}, day(2060, 1, 1)},
		// Historical rule change (US DST rules changed in 2007).
		{"America/New_York", Coverage{Start: day(2000, 1, 1), End: day(2012, 1, 1)}, day(2012, 1, 1)},
		{"America/New_York", Coverage{Start: day(2000, 1, 1), Unbounded: true}, day(2099, 12, 1)},
		// Fixed offset (no DST since 1951).
		{"Asia/Tokyo", Coverage{Start: day(2026, 1, 1), Unbounded: true}, day(2099, 12, 1)},
		// Southern hemisphere.
		{"Australia/Sydney", Coverage{Start: day(2026, 1, 1), Unbounded: true}, day(2099, 12, 1)},
		// 30-minute DST.
		{"Australia/Lord_Howe", Coverage{Start: day(2026, 1, 1), Unbounded: true}, day(2099, 12, 1)},
		// Rules that change outside 00:00–24:00 (Friday after the fourth
		// Thursday at "26:00"; Saturday 23:00 before the last Sunday).
		{"Asia/Jerusalem", Coverage{Start: day(2026, 1, 1), Unbounded: true}, day(2099, 12, 1)},
		{"America/Nuuk", Coverage{Start: day(2026, 1, 1), Unbounded: true}, day(2099, 12, 1)},
		// "24:00 on the last Thursday of October", which can be 1 November.
		{"Africa/Cairo", Coverage{Start: day(2026, 1, 1), Unbounded: true}, day(2099, 12, 1)},
		// Negative DST (winter is the "daylight" observance).
		{"Europe/Dublin", Coverage{Start: day(2026, 1, 1), Unbounded: true}, day(2099, 12, 1)},
	}
	for _, c := range cases {
		loc := mustLoc(t, c.zone)
		comp, err := GenerateVTimezone(c.zone, c.cov)
		if err != nil {
			t.Fatalf("%s: %v", c.zone, err)
		}
		checkAgainstLocation(t, comp, loc, c.cov.Start, c.to)
	}
}

func TestGeneratedVTimezoneIsValidICalendar(t *testing.T) {
	comp, err := GenerateVTimezone("Europe/Zurich", Coverage{Start: day(2026, 3, 13), Unbounded: true})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	encodeComponent(&b, comp)
	text := b.String()
	for _, want := range []string{"TZID:Europe/Zurich\r\n", "TZOFFSETFROM:+0100\r\n", "RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU\r\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "VALUE=") {
		t.Errorf("offsets must carry no VALUE parameter:\n%s", text)
	}
	cal, err := Decode([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\n" + text + "END:VCALENDAR\r\n"))
	if err != nil || len(cal.Children) != 1 {
		t.Fatalf("re-decoding: %v", err)
	}
}

func TestVTimezoneCacheStaysSmall(t *testing.T) {
	vtzCache.Lock()
	vtzCache.m = map[string]vtzCacheEntry{}
	vtzCache.Unlock()
	const tz = "Europe/Zurich"
	// One-off objects on many different days share one definition.
	first, err := GenerateVTimezone(tz, Coverage{Start: day(2026, 1, 5), End: day(2026, 1, 6)})
	if err != nil {
		t.Fatal(err)
	}
	for d := 0; d < 200; d++ {
		c := day(2026, 1, 6).AddDate(0, 0, d)
		got, err := GenerateVTimezone(tz, Coverage{Start: c, End: c.AddDate(0, 0, 1)})
		if err != nil || got != first {
			t.Fatalf("day %d: a new definition (err %v)", d, err)
		}
	}
	// An unbounded series from the same year is covered too.
	if got, _ := GenerateVTimezone(tz, Coverage{Start: day(2026, 3, 13), Unbounded: true}); got != first {
		t.Error("unbounded coverage in the cached span generated anew")
	}
	// An earlier start replaces the entry (still one per zone) and covers both.
	earlier, err := GenerateVTimezone(tz, Coverage{Start: day(2020, 6, 1), End: day(2020, 6, 2)})
	if err != nil || earlier == first {
		t.Fatalf("earlier start reused a narrower definition (err %v)", err)
	}
	checkAgainstLocation(t, earlier, mustLoc(t, tz), day(2020, 6, 1), day(2030, 1, 1))
	vtzCache.Lock()
	n := len(vtzCache.m)
	vtzCache.Unlock()
	if n != 1 {
		t.Fatalf("cache holds %d entries, want 1", n)
	}
	// A definition cached under other zone data (another tzdb) is never used.
	stale := goical.NewComponent(goical.CompTimezone)
	vtzCache.Lock()
	vtzCache.m["Asia/Tokyo"] = vtzCacheEntry{cov: Coverage{Start: day(1900, 1, 1), Unbounded: true}, comp: stale, print: "other-tzdb"}
	vtzCache.Unlock()
	if b, err := GenerateVTimezone("Asia/Tokyo", Coverage{Start: day(2026, 1, 1), Unbounded: true}); err != nil || b == stale {
		t.Errorf("stale-tzdb entry reused (err %v)", err)
	}
}

func TestRuleParts(t *testing.T) {
	cases := []struct {
		name string
		m    mRule
		want []string
	}{
		// Jerusalem: 02:00 on the Friday after the fourth Thursday of March.
		{"shift inside the month", mRule{3, 4, 4, 26 * 3600},
			[]string{"FREQ=YEARLY;BYMONTH=3;BYDAY=FR;BYMONTHDAY=23,24,25,26,27,28,29"}},
		// Cairo: 24:00 on the last Thursday of October — the Friday after,
		// which is 1 November when the last Thursday is the 31st.
		{"last week into the next month", mRule{10, 5, 4, 24 * 3600}, []string{
			"FREQ=YEARLY;BYMONTH=10;BYDAY=FR;BYMONTHDAY=-6,-5,-4,-3,-2,-1",
			"FREQ=YEARLY;BYMONTH=11;BYDAY=FR;BYMONTHDAY=1"}},
		// Nuuk: 23:00 on the Saturday before the last Sunday of March.
		{"last week, a day earlier", mRule{3, 5, 0, -3600},
			[]string{"FREQ=YEARLY;BYMONTH=3;BYDAY=SA;BYMONTHDAY=-8,-7,-6,-5,-4,-3,-2"}},
		// The day before the first Sunday of March: late February included,
		// counted from February's end so leap years don't matter.
		{"first week into the previous month", mRule{3, 1, 0, -3600}, []string{
			"FREQ=YEARLY;BYMONTH=2;BYDAY=SA;BYMONTHDAY=-1",
			"FREQ=YEARLY;BYMONTH=3;BYDAY=SA;BYMONTHDAY=1,2,3,4,5,6"}},
		// Last Sunday of February moved a day earlier: fine with negative days.
		{"last week of February", mRule{2, 5, 0, -3600},
			[]string{"FREQ=YEARLY;BYMONTH=2;BYDAY=SA;BYMONTHDAY=-8,-7,-6,-5,-4,-3,-2"}},
	}
	for _, c := range cases {
		parts, err := c.m.parts()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []string
		for _, p := range parts {
			got = append(got, p.rrule)
		}
		if strings.Join(got, " | ") != strings.Join(c.want, " | ") {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, c.want)
		}
	}
	// The fourth Sunday of February plus two days runs past the 28th, which
	// only exists in leap years.
	if _, err := (mRule{2, 4, 0, 2 * 86400}).parts(); err == nil {
		t.Error("a leap-year-dependent rule rendered")
	}
}

const bareTZID = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//calclient//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:a\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;TZID=Europe/Zurich:20260313T090000\r\nDTEND;TZID=Europe/Zurich:20260313T100000\r\n" +
	"RRULE:FREQ=WEEKLY\r\nEXDATE;TZID=Europe/Zurich:20260403T090000\r\nSUMMARY:s\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

func TestAddMissingVTimezones(t *testing.T) {
	cal, _ := Decode([]byte(bareTZID))
	out, err := AddMissingVTimezones([]byte(bareTZID), cal)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// Inserted before the VEVENT; every original line kept byte for byte.
	if i, j := strings.Index(s, "BEGIN:VTIMEZONE"), strings.Index(s, "BEGIN:VEVENT"); i < 0 || i > j {
		t.Fatalf("VTIMEZONE not inserted before the event:\n%s", s)
	}
	stripped := s[:strings.Index(s, "BEGIN:VTIMEZONE")] + s[strings.Index(s, "END:VTIMEZONE\r\n")+len("END:VTIMEZONE\r\n"):]
	if stripped != bareTZID {
		t.Fatalf("rest of the body changed:\n%s", stripped)
	}
	// An unbounded series gets open-ended observances.
	if !strings.Contains(s, "RRULE:FREQ=YEARLY") {
		t.Errorf("no open-ended observances for an unbounded series:\n%s", s)
	}

	// Already defined, UTC-only, or unknown zones: nothing to add.
	cal2, _ := Decode(out)
	if again, _ := AddMissingVTimezones(out, cal2); !bytes.Equal(again, out) {
		t.Error("a defined TZID got a second VTIMEZONE")
	}
	outlook := strings.ReplaceAll(bareTZID, "Europe/Zurich", "W. Europe Standard Time")
	cal3, _ := Decode([]byte(outlook))
	if same, _ := AddMissingVTimezones([]byte(outlook), cal3); string(same) != outlook {
		t.Error("an unknown zone got a generated definition")
	}
}

func TestAddMissingVTimezonesCoverage(t *testing.T) {
	var got []Coverage
	defer func(g func(string, Coverage) (*goical.Component, error)) { generateVTimezone = g }(generateVTimezone)
	generateVTimezone = func(tz string, c Coverage) (*goical.Component, error) {
		got = append(got, c)
		return GenerateVTimezone(tz, c)
	}
	bounded := strings.Replace(bareTZID, "RRULE:FREQ=WEEKLY", "RRULE:FREQ=WEEKLY;UNTIL=20270101T000000Z", 1)
	cal, _ := Decode([]byte(bounded))
	if _, err := AddMissingVTimezones([]byte(bounded), cal); err != nil {
		t.Fatal(err)
	}
	c := got[0]
	if c.Unbounded || !c.Start.Equal(time.Date(2026, 3, 13, 8, 0, 0, 0, time.UTC)) || c.End.Before(day(2027, 1, 1)) {
		t.Fatalf("coverage = %+v, want 13 March 2026 through UNTIL", c)
	}
}

func TestAddMissingVTimezonesFailsWithoutPartialOutput(t *testing.T) {
	defer func(g func(string, Coverage) (*goical.Component, error)) { generateVTimezone = g }(generateVTimezone)
	generateVTimezone = func(string, Coverage) (*goical.Component, error) {
		return nil, errors.New("its daylight-saving rule cannot be expressed")
	}
	cal, _ := Decode([]byte(bareTZID))
	out, err := AddMissingVTimezones([]byte(bareTZID), cal)
	if err == nil || out != nil {
		t.Fatalf("= %q, %v; want an error and no body", out, err)
	}
}
