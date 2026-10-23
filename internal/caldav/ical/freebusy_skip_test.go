package ical

import (
	"strings"
	"testing"
	"time"
)

// One stored object with an unparseable date (sabre is more lenient on
// write) must not 500 the whole free-busy answer; it is skipped (audit 6.14).
func TestFreeBusySkipsUnusableObject(t *testing.T) {
	good := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:g\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260601T100000Z\r\nDTEND:20260601T110000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	bad := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:b\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260601T120000Z\r\nDURATION:PTBOGUS\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	out, err := GenerateFreeBusy([][]byte{[]byte(bad), []byte(good)}, start, start.AddDate(0, 0, 1), time.UTC, start)
	if err != nil {
		t.Fatalf("GenerateFreeBusy failed on one bad object: %v", err)
	}
	if !strings.Contains(string(out), "FREEBUSY:20260601T100000Z/20260601T110000Z") {
		t.Errorf("good object's period missing:\n%s", out)
	}
}
