package caldav_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/dav/mock"
)

const privateEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:dentist\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260601T100000Z\r\nDTEND:20260601T110000Z\r\nRRULE:FREQ=WEEKLY;COUNT=3\r\n" +
	"SUMMARY:Dentist\r\nDESCRIPTION:Root canal\r\nLOCATION:Clinic\r\nCLASS:PRIVATE\r\n" +
	"ATTENDEE:mailto:nurse@example.org\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// sharedExportServer: user1 owns calendar "1"; user2 holds a read-only
// shared instance of it (calendar "2", marked the way the storage backend
// marks ACCESS_READ shares). Both contain the same CLASS:PRIVATE event.
func sharedExportServer(loggedIn string) *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
		{ID: caldav.CalendarID{Calendar: "2"}, URI: "shared", PrincipalURI: "principals/user2",
			Props: map[xmlNM]any{{Space: "http://sabredav.org/ns", Local: "read-only"}: "1"}},
	}
	obj := &caldav.ObjectInfo{URI: "dentist.ics", CalendarData: []byte(privateEvent)}
	backend.Objects = map[caldav.CalendarID]map[string]*caldav.ObjectInfo{
		{Calendar: "1"}: {"dentist.ics": obj},
		{Calendar: "2"}: {"dentist.ics": obj},
	}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(loggedIn))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	return srv
}

// GET ?export used to read the backend directly and so bypassed the
// CLASS:PRIVATE redaction that a normal GET applies on a read-only share
// (audit 3.4).
func TestExportRedactsRestrictedOnReadOnlyShare(t *testing.T) {
	export := func(user, path string) string {
		t.Helper()
		srv := sharedExportServer(user)
		get := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, get)
		if w.Code != 200 {
			t.Fatalf("%s %s = %d:\n%s", user, path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	for _, path := range []string{
		"/calendars/user2/shared/?export",
		"/calendars/user2/shared/?export&expand=1&start=1780000000&end=1790000000",
	} {
		body := export("principals/user2", path)
		if !strings.Contains(body, "SUMMARY:Busy") || !strings.Contains(body, "UID:dentist") {
			t.Errorf("%s: sharee export should contain the opaque block:\n%s", path, body)
		}
		for _, leak := range []string{"Dentist", "Root canal", "Clinic", "nurse@example.org"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s: sharee export leaks %q:\n%s", path, leak, body)
			}
		}
	}

	body := export("principals/user1", "/calendars/user1/default/?export")
	if !strings.Contains(body, "SUMMARY:Dentist") || !strings.Contains(body, "Root canal") {
		t.Errorf("owner export must be unredacted:\n%s", body)
	}
}
