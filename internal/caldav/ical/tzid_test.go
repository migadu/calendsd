package ical

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// outlookSeries is a weekly 09:00 series across the 29 March 2026 switch in an
// Outlook-style zone (with the CET/CEST VTIMEZONE Outlook embeds): Mar 20 is
// excluded and Apr 3 moved to 11:00.
func outlookSeries(tzid string) []byte {
	q := tzid
	if strings.ContainsAny(tzid, ",:;") {
		q = `"` + tzid + `"`
	}
	return ics(
		"BEGIN:VTIMEZONE", "TZID:"+tzid,
		"BEGIN:STANDARD", "DTSTART:16011028T030000",
		"TZOFFSETFROM:+0200", "TZOFFSETTO:+0100",
		"RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10", "END:STANDARD",
		"BEGIN:DAYLIGHT", "DTSTART:16010325T020000",
		"TZOFFSETFROM:+0100", "TZOFFSETTO:+0200",
		"RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3", "END:DAYLIGHT",
		"END:VTIMEZONE",
		"BEGIN:VEVENT", "UID:outlook", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID="+q+":20260313T090000",
		"DTEND;TZID="+q+":20260313T100000",
		"RRULE:FREQ=WEEKLY;COUNT=5",
		"EXDATE;TZID="+q+":20260320T090000",
		"END:VEVENT",
		"BEGIN:VEVENT", "UID:outlook", "DTSTAMP:20260101T000000Z",
		"RECURRENCE-ID;TZID="+q+":20260403T090000",
		"DTSTART;TZID="+q+":20260403T110000",
		"DTEND;TZID="+q+":20260403T120000",
		"END:VEVENT",
	)
}

// An Outlook TZID used to be flattened to UTC before expansion, so the rule
// repeated in UTC: after the DST switch every occurrence moved to 10:00 local
// and the override (at 09:00 CEST = 07:00Z) no longer matched its slot.
func TestExpandOutlookTZIDKeepsWallClock(t *testing.T) {
	for _, tzid := range []string{
		"W. Europe Standard Time",                   // Windows zone table
		"(UTC+01:00) Amsterdam, Berlin, Bern, Rome", // matched by its rules
	} {
		t.Run(tzid, func(t *testing.T) {
			cal, err := Decode(outlookSeries(tzid))
			if err != nil {
				t.Fatal(err)
			}
			out, err := Expand(cal, utc("20260301T000000Z"), utc("20260501T000000Z"), time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range out.Children {
				got = append(got, c.Props.Get("RECURRENCE-ID").Value+" "+c.Props.Get("DTSTART").Value)
			}
			want := []string{
				"20260313T080000Z 20260313T080000Z",
				"20260327T080000Z 20260327T080000Z",
				"20260403T070000Z 20260403T090000Z",
				"20260410T070000Z 20260410T070000Z",
			}
			if !slices.Equal(got, want) {
				t.Errorf("instances:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// A Windows zone name resolves through the table even without a VTIMEZONE.
func TestDenormalizeWindowsTZIDWithoutVTimezone(t *testing.T) {
	checkBounds(t, event(
		"DTSTART;TZID=Pacific Standard Time:20260701T090000",
		"DTEND;TZID=Pacific Standard Time:20260701T100000",
	), ts(t, 2026, 7, 1, 16, 0), ts(t, 2026, 7, 1, 17, 0))
}

// A zone the table doesn't know whose VTIMEZONE has no DST is not matched to
// an IANA zone (that zone's DST could apply outside the comparison window);
// its values are converted with the VTIMEZONE's own offset.
func TestFixedOffsetCustomTZIDIsFlattened(t *testing.T) {
	cal, err := Decode(ics(
		"BEGIN:VTIMEZONE", "TZID:Custom Fixed",
		"BEGIN:STANDARD", "DTSTART:16010101T000000",
		"TZOFFSETFROM:+0530", "TZOFFSETTO:+0530", "END:STANDARD",
		"END:VTIMEZONE",
		"BEGIN:VEVENT", "UID:fixed", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=Custom Fixed:20260601T100000",
		"END:VEVENT",
	))
	if err != nil {
		t.Fatal(err)
	}
	resolveNonIANATZIDs(cal)
	ds := cal.Children[1].Props.Get("DTSTART")
	if ds.Value != "20260601T043000Z" || ds.Params.Get("TZID") != "" {
		t.Errorf("DTSTART = %v %q, want 20260601T043000Z without TZID", ds.Params, ds.Value)
	}
}

func TestWindowsZonesLoad(t *testing.T) {
	for win, iana := range windowsZones {
		if _, err := time.LoadLocation(iana); err != nil {
			t.Errorf("%s -> %s: %v", win, iana, err)
		}
	}
}
