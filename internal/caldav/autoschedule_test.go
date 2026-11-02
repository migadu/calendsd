package caldav_test

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/dav/mock"
)

// newAutoScheduleServer builds a server with an organizer (user1) and an
// auto-scheduling resource principal (resource1), each with one calendar.
func newAutoScheduleServer() (*dav.Server, *caldav.MockSchedulingBackend) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{
			{Space: "http://sabredav.org/ns", Local: "email-address"}: "user1@example.org",
		}},
		{URI: "principals/resource1", Props: map[xmlNM]any{
			{Space: "http://sabredav.org/ns", Local: "email-address"}: "resource1@example.org",
		}},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
		{ID: caldav.CalendarID{Calendar: "2"}, URI: "default", PrincipalURI: "principals/resource1"},
	}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))

	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{
		AutoSchedule: func(principalURI string) bool {
			return strings.HasPrefix(principalURI, "principals/resource")
		},
	})
	return srv, backend
}

func bookingICS(uid, start, end string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:" + start + "\r\nDTEND:" + end + "\r\nSUMMARY:Booking\r\n" +
		"ORGANIZER;CN=User One:mailto:user1@example.org\r\n" +
		"ATTENDEE;CN=User One;PARTSTAT=ACCEPTED:mailto:user1@example.org\r\n" +
		"ATTENDEE;CN=Room;CUTYPE=RESOURCE:mailto:resource1@example.org\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
}

// TestAutoScheduleAccepts: a resource attendee auto-accepts a booking — the
// copy lands on its calendar as ACCEPTED, the organizer's object gets
// PARTSTAT=ACCEPTED + SCHEDULE-STATUS=2.0 from the REPLY, and the REPLY is
// dropped into the organizer's inbox (no inbox item for the resource).
func TestAutoScheduleAccepts(t *testing.T) {
	srv, backend := newAutoScheduleServer()

	req := httptest.NewRequest("PUT", "/calendars/user1/default/b1.ics",
		strings.NewReader(bookingICS("book-1", "20260901T100000Z", "20260901T110000Z")))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}

	// Resource copy, auto-accepted.
	resourceObjs := backend.Objects[caldav.CalendarID{Calendar: "2"}]
	if len(resourceObjs) != 1 {
		t.Fatalf("resource calendar has %d objects, want 1", len(resourceObjs))
	}
	for _, obj := range resourceObjs {
		data := string(obj.CalendarData)
		if !strings.Contains(data, "PARTSTAT=ACCEPTED:mailto:resource1@example.org") &&
			!strings.Contains(strings.ReplaceAll(data, "\r\n ", ""), "PARTSTAT=ACCEPTED:mailto:resource1@example.org") {
			t.Errorf("resource copy not accepted:\n%s", data)
		}
	}

	// Organizer copy reflects the auto-reply.
	get := httptest.NewRequest("GET", "/calendars/user1/default/b1.ics", nil)
	gw := httptest.NewRecorder()
	srv.ServeHTTP(gw, get)
	body := strings.ReplaceAll(gw.Body.String(), "\r\n ", "") // unfold
	if !strings.Contains(body, "SCHEDULE-STATUS=2.0") {
		t.Errorf("organizer copy missing SCHEDULE-STATUS=2.0:\n%s", gw.Body.String())
	}
	attLine := ""
	for _, line := range strings.Split(body, "\r\n") {
		if strings.Contains(line, "resource1@example.org") && strings.HasPrefix(line, "ATTENDEE") {
			attLine = line
		}
	}
	if !strings.Contains(attLine, "PARTSTAT=ACCEPTED") {
		t.Errorf("resource attendee not ACCEPTED on organizer copy: %s", attLine)
	}

	// REPLY in the organizer's inbox; nothing in the resource's inbox.
	if n := len(backend.Scheduling["principals/user1"]); n != 1 {
		t.Errorf("organizer inbox has %d items, want 1 (the REPLY)", n)
	}
	if n := len(backend.Scheduling["principals/resource1"]); n != 0 {
		t.Errorf("resource inbox has %d items, want 0 (auto-processed)", n)
	}
}

// TestAutoScheduleDeclinesOnConflict: a second booking overlapping an
// accepted one is declined and does not land on the resource calendar.
func TestAutoScheduleDeclinesOnConflict(t *testing.T) {
	srv, backend := newAutoScheduleServer()

	for i, tc := range []struct {
		uid, start, end string
		wantPartStat    string
	}{
		{"book-1", "20260901T100000Z", "20260901T110000Z", "PARTSTAT=ACCEPTED"},
		{"book-2", "20260901T103000Z", "20260901T113000Z", "PARTSTAT=DECLINED"}, // overlaps
		{"book-3", "20260901T110000Z", "20260901T120000Z", "PARTSTAT=ACCEPTED"}, // adjacent
	} {
		path := fmt.Sprintf("/calendars/user1/default/c%d.ics", i)
		req := httptest.NewRequest("PUT", path,
			strings.NewReader(bookingICS(tc.uid, tc.start, tc.end)))
		req.Header.Set("Content-Type", "text/calendar")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 201 {
			t.Fatalf("PUT %s = %d: %s", tc.uid, w.Code, w.Body.String())
		}

		get := httptest.NewRequest("GET", path, nil)
		gw := httptest.NewRecorder()
		srv.ServeHTTP(gw, get)
		body := strings.ReplaceAll(gw.Body.String(), "\r\n ", "")
		attLine := ""
		for _, line := range strings.Split(body, "\r\n") {
			if strings.Contains(line, "resource1@example.org") && strings.HasPrefix(line, "ATTENDEE") {
				attLine = line
			}
		}
		if !strings.Contains(attLine, tc.wantPartStat) {
			t.Errorf("%s: resource attendee = %q, want %s", tc.uid, attLine, tc.wantPartStat)
		}
	}

	// Only the two accepted bookings are on the resource calendar.
	if n := len(backend.Objects[caldav.CalendarID{Calendar: "2"}]); n != 2 {
		t.Errorf("resource calendar has %d objects, want 2 (declined booking excluded)", n)
	}
}

// TestCalendarUserTypeRoom: an auto-schedule principal reports CUTYPE=ROOM so a
// client can flag it as a bookable room in the attendee picker; an ordinary
// principal stays INDIVIDUAL.
func TestCalendarUserTypeRoom(t *testing.T) {
	principals := &acl.MemoryBackend{
		Principals: []acl.Principal{
			{URI: "principals/user1", Props: map[xmlNM]any{
				{Space: "http://sabredav.org/ns", Local: "email-address"}: "user1@example.org",
			}},
			{URI: "principals/resource1", Props: map[xmlNM]any{
				{Space: "http://sabredav.org/ns", Local: "email-address"}: "resource1@example.org",
			}},
			{URI: "principals/team1", Props: map[xmlNM]any{
				{Space: "http://sabredav.org/ns", Local: "email-address"}: "team1@example.org",
			}},
		},
		Groups: map[string][]string{"principals/team1": {"principals/user1"}},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}, PrincipalsReadableByAuthenticated: true})
	srv.AddPlugin(&caldav.SchedulePlugin{
		AutoSchedule: func(uri string) bool { return strings.HasPrefix(uri, "principals/resource") },
	})

	cuType := func(path string) string {
		body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-user-type/></d:prop></d:propfind>`
		req := httptest.NewRequest("PROPFIND", path, strings.NewReader(body))
		req.Header.Set("Depth", "0")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 207 {
			t.Fatalf("PROPFIND %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if out := cuType("/principals/resource1/"); !strings.Contains(out, "<cal:calendar-user-type>ROOM") && !strings.Contains(out, ">ROOM<") {
		t.Errorf("resource principal calendar-user-type: want ROOM in\n%s", out)
	}
	if out := cuType("/principals/user1/"); !strings.Contains(out, "INDIVIDUAL") {
		t.Errorf("user principal calendar-user-type: want INDIVIDUAL in\n%s", out)
	}
	if out := cuType("/principals/team1/"); !strings.Contains(out, ">GROUP<") && !strings.Contains(out, "user-type>GROUP") {
		t.Errorf("group principal calendar-user-type: want GROUP in\n%s", out)
	}
}
