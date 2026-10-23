package caldav_test

// Audit 4.6: the auto-schedule conflict check compared the booking's whole
// occurrence hull (DTSTART→2038 for an open-ended rule) against the room's
// busy time, so a standing booking was declined if the room had anything at
// all. It now checks each instance within a one-year horizon.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"calendsd/internal/caldav"
)

func weeklyBookingICS(uid, start, end string) string {
	return strings.Replace(bookingICS(uid, start, end),
		"SUMMARY:Booking\r\n", "SUMMARY:Standing booking\r\nRRULE:FREQ=WEEKLY\r\n", 1)
}

func resourceAttendeeLine(t *testing.T, body string) string {
	t.Helper()
	body = strings.ReplaceAll(body, "\r\n ", "")
	for _, line := range strings.Split(body, "\r\n") {
		if strings.HasPrefix(line, "ATTENDEE") && strings.Contains(line, "resource1@example.org") {
			return line
		}
	}
	t.Fatalf("no resource ATTENDEE in:\n%s", body)
	return ""
}

func TestAutoScheduleStandingBookingCheckedPerInstance(t *testing.T) {
	for _, tc := range []struct {
		name            string
		roomStart, room string // an existing room event
		want            string
	}{
		// Sundays from 2026-11-01 10:00–11:00; a Thursday event does not clash.
		{"unrelated event next month", "20261203T100000Z", "20261203T110000Z", "PARTSTAT=ACCEPTED"},
		// Overlaps the third instance (2026-11-15).
		{"event overlapping the third instance", "20261115T103000Z", "20261115T113000Z", "PARTSTAT=DECLINED"},
		// Beyond the one-year horizon: accepted unchecked.
		{"event two years out", "20281105T100000Z", "20281105T110000Z", "PARTSTAT=ACCEPTED"},
	} {
		srv, backend := newAutoScheduleServer()
		backend.Objects[caldav.CalendarID{Calendar: "2"}] = map[string]*caldav.ObjectInfo{
			"existing.ics": {URI: "existing.ics", CalendarData: []byte(bookingICS("existing", tc.roomStart, tc.room))},
		}

		req := httptest.NewRequest("PUT", "/calendars/user1/default/standing.ics",
			strings.NewReader(weeklyBookingICS("standing", "20261101T100000Z", "20261101T110000Z")))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 201 {
			t.Fatalf("%s: PUT = %d: %s", tc.name, w.Code, w.Body.String())
		}
		get := httptest.NewRequest("GET", "/calendars/user1/default/standing.ics", nil)
		gw := httptest.NewRecorder()
		srv.ServeHTTP(gw, get)
		if line := resourceAttendeeLine(t, gw.Body.String()); !strings.Contains(line, tc.want) {
			t.Errorf("%s: resource attendee = %q, want %s", tc.name, line, tc.want)
		}
	}
}
