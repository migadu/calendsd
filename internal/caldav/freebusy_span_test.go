package caldav_test

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// statusCount counts recipients answered with the given status code prefix,
// whatever namespace prefix the serializer uses.
func statusCount(body, code string) int {
	return len(regexp.MustCompile(`<(?:[a-z]+:)?request-status[^>]*>`+regexp.QuoteMeta(code)+`;`).FindAllString(body, -1))
}

// The free-busy window is client-chosen and drives a full recurrence
// expansion, so it is capped at 366 days and never silently truncated
// (audit 3.1, D1).
func TestFreeBusyQueryReportSpanCap(t *testing.T) {
	srv := freeBusyServer("principals/user1")
	report := func(attrs string) *httptest.ResponseRecorder {
		return davRequest(t, srv, "REPORT", "/calendars/user1/default/",
			`<?xml version="1.0"?><c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav">`+
				`<c:time-range `+attrs+`/></c:free-busy-query>`, "Depth", "1")
	}
	for _, tc := range []struct {
		name  string
		attrs string
		want  int
	}{
		{"365 days", `start="20260101T000000Z" end="20270101T000000Z"`, 200},
		{"366 days", `start="20260101T000000Z" end="20270102T000000Z"`, 200},
		{"400 days", `start="20260101T000000Z" end="20270205T000000Z"`, 403},
		{"open end", `start="20260101T000000Z"`, 403},
		{"open start", `end="20270101T000000Z"`, 403},
		{"no bounds at all", ``, 403},
	} {
		w := report(tc.attrs)
		if w.Code != tc.want {
			t.Errorf("%s: status %d, want %d:\n%s", tc.name, w.Code, tc.want, w.Body.String())
		}
		if tc.want == 403 && !strings.Contains(w.Body.String(), "366") {
			t.Errorf("%s: 403 body should name the limit:\n%s", tc.name, w.Body.String())
		}
	}
}

// An outbox VFREEBUSY wider than the cap answers every recipient with a
// terminal 5.3 inside a normal schedule-response.
func TestFreeBusyOutboxSpanCap(t *testing.T) {
	srv := freeBusyServer("principals/user2")
	post := func(dtstart, dtend string) *httptest.ResponseRecorder {
		vfb := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REQUEST\r\n" +
			"BEGIN:VFREEBUSY\r\nUID:fb-probe\r\nDTSTART:" + dtstart + "\r\nDTEND:" + dtend + "\r\n" +
			"ORGANIZER:mailto:user2@example.org\r\n" +
			"ATTENDEE:mailto:user1@example.org\r\n" +
			"ATTENDEE:mailto:nobody@example.org\r\n" +
			"END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
		req := httptest.NewRequest("POST", "/calendars/user2/outbox", strings.NewReader(vfb))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	w := post("20260101T000000Z", "20360101T000000Z")
	if w.Code != 200 {
		t.Fatalf("ten-year outbox request = %d, want 200 schedule-response:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if n := statusCount(body, "5.3"); n != 2 {
		t.Errorf("want both recipients at 5.3, got %d:\n%s", n, body)
	}
	if strings.Contains(body, "calendar-data") {
		t.Errorf("no free-busy data may be returned for an over-wide window:\n%s", body)
	}

	w = post("20260801T000000Z", "20261001T000000Z")
	if w.Code != 200 || statusCount(w.Body.String(), "2.0") != 1 {
		t.Errorf("two-month request should still succeed: %d\n%s", w.Code, w.Body.String())
	}

	w = post("20261001T000000Z", "20260801T000000Z")
	if w.Code != 200 || statusCount(w.Body.String(), "5.3") != 2 || strings.Contains(w.Body.String(), "calendar-data") {
		t.Errorf("inverted window should answer every recipient 5.3 with no data: %d\n%s", w.Code, w.Body.String())
	}
}
