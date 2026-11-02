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

// A calendar PROPFIND advertises sabre's constant calendar properties.
func TestCalendarConstantProps(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	backend := &caldav.MockBackend{Calendars: []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1",
	}}}

	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})

	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop>` +
		`<c:max-resource-size/><c:supported-calendar-data/><c:supported-collation-set/></d:prop></d:propfind>`
	req := httptest.NewRequest("PROPFIND", "/calendars/user1/default/", strings.NewReader(body))
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != 207 {
		t.Fatalf("PROPFIND = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		">10000000<",
		`content-type="text/calendar"`,
		"i;ascii-casemap",
		"i;octet",
		"i;unicode-casemap",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("calendar PROPFIND missing %q:\n%s", want, out)
		}
	}
}
