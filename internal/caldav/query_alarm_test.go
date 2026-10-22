package caldav

import (
	"testing"
	"time"

	"calserver/internal/caldav/ical"
)

// An RDATE-only event recurs, so a VALARM time-range must look at the alarms
// of every occurrence, not only the first.
func TestValarmTimeRangeOnRDATEOnlyEvent(t *testing.T) {
	cal, err := ical.Decode([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:rdate\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260105T100000Z\r\nDTEND:20260105T110000Z\r\nRDATE:20260301T100000Z\r\n" +
		"BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:x\r\nTRIGGER:-PT15M\r\nEND:VALARM\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the RDATE occurrence's alarm (09:45 on March 1) is in the window.
	filter := CompFilter{Name: "VCALENDAR", CompFilters: []CompFilter{{
		Name: "VEVENT", CompFilters: []CompFilter{{
			Name: "VALARM",
			TimeRange: &TimeRange{
				Start: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
				End:   time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			},
		}},
	}}}
	match, err := (&Validator{Loc: time.UTC}).Validate(cal, filter)
	if err != nil || !match {
		t.Errorf("VALARM query on the RDATE occurrence = %v, %v; want a match", match, err)
	}
}
