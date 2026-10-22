package caldav_test

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

// MKCALENDAR properties are read namespace-aware: text is unescaped, element
// values are stored self-contained, and a prefixed resourcetype resolves to
// the right names.
func TestMkcalendarPropsNamespaceAware(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	backend := &caldav.MockBackend{}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})

	body := `<?xml version="1.0"?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:set><D:prop>
<D:displayname>A &amp; B</D:displayname>
<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>
<C:supported-calendar-component-set><C:comp name="VTODO"/></C:supported-calendar-component-set>
</D:prop></D:set></C:mkcalendar>`
	req := httptest.NewRequest("MKCALENDAR", "/calendars/user1/new/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("MKCALENDAR = %d: %s", rec.Code, rec.Body.String())
	}
	if len(backend.Calendars) != 1 {
		t.Fatalf("calendars = %d, want 1", len(backend.Calendars))
	}
	props := backend.Calendars[0].Props
	if got := props[xml.Name{Space: "DAV:", Local: "displayname"}]; got != "A & B" {
		t.Errorf("displayname = %#v, want %q", got, "A & B")
	}
	comps, _ := props[xml.Name{Space: "urn:ietf:params:xml:ns:caldav", Local: "supported-calendar-component-set"}].([]byte)
	if !strings.Contains(string(comps), `xmlns="urn:ietf:params:xml:ns:caldav"`) || strings.Contains(string(comps), "C:") {
		t.Errorf("component set fragment is not self-contained: %q", comps)
	}
	if _, stored := props[xml.Name{Space: "DAV:", Local: "resourcetype"}]; stored {
		t.Error("resourcetype stored as a property")
	}
}

func TestMkcalendarRejectsWrongRoot(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/user1"}}}
	backend := &caldav.MockBackend{}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user1"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})

	body := `<D:mkcol xmlns:D="DAV:"><D:set><D:prop><D:displayname>x</D:displayname></D:prop></D:set></D:mkcol>`
	req := httptest.NewRequest("MKCALENDAR", "/calendars/user1/new/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("MKCALENDAR with a mkcol root = %d, want 400", rec.Code)
	}
}
