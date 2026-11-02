package caldav_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/dav/mock"
)

func tzProp(tzid string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nBEGIN:VTIMEZONE\r\nTZID:" + tzid +
		"\r\nBEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR\r\n"
}

// multigetServer gives user1 two calendars in different timezones, each
// holding one floating-time event.
func multigetServer() *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	tz := xmlNM{Space: "urn:ietf:params:xml:ns:caldav", Local: "calendar-timezone"}
	backend := &caldav.MockBackend{Calendars: []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "berlin", PrincipalURI: "principals/user1",
			Props: map[xmlNM]any{tz: tzProp("Europe/Berlin")}},
		{ID: caldav.CalendarID{Calendar: "2"}, URI: "newyork", PrincipalURI: "principals/user1",
			Props: map[xmlNM]any{tz: tzProp("America/New_York")}},
	}}
	event := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nBEGIN:VEVENT\r\nUID:floating\r\n" +
		"DTSTAMP:20260101T000000Z\r\nDTSTART:20260301T100000\r\nDTEND:20260301T110000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for _, id := range []string{"1", "2"} {
		if _, err := backend.CreateCalendarObject(context.Background(), caldav.CalendarID{Calendar: id}, "e.ics", []byte(event)); err != nil {
			panic(err)
		}
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/dav/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	return srv
}

// TestMultigetExpandsEachHrefInItsCalendarTimezone: an expanded multiget
// spanning calendars resolves floating times in each object's own calendar
// timezone, not the first href's.
func TestMultigetExpandsEachHrefInItsCalendarTimezone(t *testing.T) {
	srv := multigetServer()
	body := `<?xml version="1.0"?><C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
<D:prop><C:calendar-data><C:expand start="20260301T000000Z" end="20260302T000000Z"/></C:calendar-data></D:prop>
<D:href>/dav/calendars/user1/berlin/e.ics</D:href>
<D:href>/dav/calendars/user1/newyork/e.ics</D:href>
</C:calendar-multiget>`
	req := httptest.NewRequest("REPORT", "/dav/calendars/user1/", strings.NewReader(body))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 207 {
		t.Fatalf("REPORT = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{"DTSTART:20260301T090000Z", "DTSTART:20260301T150000Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded multiget missing %q:\n%s", want, out)
		}
	}
}

// TestMultigetUnresolvableHrefIs404: an href outside the server's base is
// answered with a 404 response instead of being dropped.
func TestMultigetUnresolvableHrefIs404(t *testing.T) {
	srv := multigetServer()
	body := `<?xml version="1.0"?><C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
<D:prop><D:getetag/></D:prop>
<D:href>/elsewhere/e.ics</D:href>
<D:href>/dav/calendars/user1/berlin/e.ics</D:href>
</C:calendar-multiget>`
	req := httptest.NewRequest("REPORT", "/dav/calendars/user1/berlin/", strings.NewReader(body))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 207 {
		t.Fatalf("REPORT = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	i := strings.Index(out, "/elsewhere/e.ics")
	if i < 0 {
		t.Fatalf("unresolvable href missing from the response:\n%s", out)
	}
	if rest := out[i:]; !strings.Contains(rest[:strings.Index(rest, "response>")], "404") {
		t.Errorf("unresolvable href not answered 404:\n%s", out)
	}
	if !strings.Contains(out, "/dav/calendars/user1/berlin/e.ics") {
		t.Errorf("resolvable href missing:\n%s", out)
	}
}
