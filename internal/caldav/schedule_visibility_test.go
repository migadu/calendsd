package caldav_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

const nsX = "urn:x-calserver"

func setVisibility(t *testing.T, srv *dav.Server, user, inner string) string {
	t.Helper()
	body := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:x="` + nsX + `">` +
		`<d:set><d:prop><x:free-busy-visibility>` + inner + `</x:free-busy-visibility></d:prop></d:set></d:propertyupdate>`
	return davRequest(t, srv, "PROPPATCH", "/calendars/"+user+"/inbox/", body).Body.String()
}

func propfindVisibility(t *testing.T, srv *dav.Server, user string) string {
	t.Helper()
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:x="` + nsX + `">` +
		`<d:prop><x:free-busy-visibility/></d:prop></d:propfind>`
	return davRequest(t, srv, "PROPFIND", "/calendars/"+user+"/inbox/", body, "Depth", "0").Body.String()
}

// freeBusyQuery runs the per-calendar REPORT on user1's calendar.
func freeBusyQuery(t *testing.T, srv *dav.Server) (int, string) {
	t.Helper()
	w := davRequest(t, srv, "REPORT", "/calendars/user1/default/",
		`<?xml version="1.0"?><c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav">`+
			`<c:time-range start="20260901T000000Z" end="20260902T000000Z"/></c:free-busy-query>`, "Depth", "1")
	return w.Code, w.Body.String()
}

func TestFreeBusyVisibilityDefaultIsDomain(t *testing.T) {
	srv, _, _ := availabilityServer("principals/user1")
	body := propfindVisibility(t, srv, "user1")
	if !strings.Contains(body, ">domain</audience>") {
		t.Errorf("unset visibility should read as the domain default:\n%s", body)
	}
}

// "Only the people below": the owner hides free-busy from their domain and
// grants it to one person, here on another domain.
func TestFreeBusyVisibilityNobodyPlusAllowed(t *testing.T) {
	owner, backend, _ := availabilityServer("principals/user1")
	backend.Availabilities = map[string][]byte{"principals/user1": []byte(workingHours)}
	body := setVisibility(t, owner, "user1",
		`<x:audience>nobody</x:audience><x:allow><d:href>mailto:user3@elsewhere.example</d:href></x:allow>`)
	if strings.Contains(body, "403") {
		t.Fatalf("PROPPATCH rejected:\n%s", body)
	}
	got := backend.Visibilities["principals/user1"]
	// Outside the owner's domain the grant is kept as the address.
	if !got.Hidden || len(got.Allowed) != 1 || got.Allowed[0] != "mailto:user3@elsewhere.example" {
		t.Fatalf("stored visibility = %+v", got)
	}
	if body := propfindVisibility(t, owner, "user1"); !strings.Contains(body, ">nobody</audience>") ||
		!strings.Contains(body, "mailto:user3@elsewhere.example</href>") {
		t.Errorf("PROPFIND does not reflect the setting:\n%s", body)
	}

	// The same-domain colleague now gets the "unknown" answer, everywhere.
	colleague, cb, _ := availabilityServer("principals/user2")
	cb.Visibilities, cb.Availabilities = backend.Visibilities, backend.Availabilities
	if body := probeUser1(t, colleague, "user2"); !strings.Contains(body, "3.7;Could not find principal") ||
		strings.Contains(body, "FREEBUSY") {
		t.Errorf("hidden free-busy reached a colleague:\n%s", body)
	}
	if code, body := freeBusyQuery(t, colleague); code == 200 || strings.Contains(body, "FREEBUSY") {
		t.Errorf("free-busy-query by a colleague = %d:\n%s", code, body)
	}

	// The person on the allowed list sees busy time and working hours,
	// although they are on another domain.
	guest, gb, _ := availabilityServer("principals/user3")
	gb.Visibilities, gb.Availabilities = backend.Visibilities, backend.Availabilities
	body = probeUser1(t, guest, "user3")
	for _, want := range []string{"2.0;Success", "FREEBUSY:20260901T100000Z/20260901T110000Z", "BUSY-UNAVAILABLE"} {
		if !strings.Contains(body, want) {
			t.Errorf("allowed guest's probe missing %q:\n%s", want, body)
		}
	}
	// The per-calendar REPORT stays owner/delegate-only: REPORT is gated by
	// {DAV:}read before free-busy is even considered.
	if code, _ := freeBusyQuery(t, guest); code != 403 {
		t.Errorf("free-busy-query by the allowed guest = %d, want 403 ({DAV:}read gate)", code)
	}

	// The owner still sees their own.
	if code, _ := freeBusyQuery(t, owner); code != 200 {
		t.Errorf("owner's own free-busy-query = %d", code)
	}

	// Removing the setting restores the domain default.
	rm := davRequest(t, owner, "PROPPATCH", "/calendars/user1/inbox/",
		`<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:x="`+nsX+`">`+
			`<d:remove><d:prop><x:free-busy-visibility/></d:prop></d:remove></d:propertyupdate>`)
	if strings.Contains(rm.Body.String(), "403") {
		t.Fatalf("remove rejected:\n%s", rm.Body.String())
	}
	if _, set := backend.Visibilities["principals/user1"]; set {
		t.Error("remove left the setting in place")
	}
	if body := probeUser1(t, colleague, "user2"); !strings.Contains(body, "2.0;Success") {
		t.Errorf("after remove the colleague should see free-busy again:\n%s", body)
	}
}

// An allowed list without "nobody" adds people on top of the domain.
func TestFreeBusyVisibilityDomainPlusAllowed(t *testing.T) {
	owner, backend, _ := availabilityServer("principals/user1")
	if body := setVisibility(t, owner, "user1", `<x:allow><d:href>mailto:USER3@elsewhere.example</d:href></x:allow>`); strings.Contains(body, "403") {
		t.Fatalf("PROPPATCH rejected:\n%s", body)
	}
	for _, user := range []string{"user2", "user3"} {
		srv, b, _ := availabilityServer("principals/" + user)
		b.Visibilities = backend.Visibilities
		if body := probeUser1(t, srv, user); !strings.Contains(body, "2.0;Success") {
			t.Errorf("%s should see free-busy:\n%s", user, body)
		}
	}
}

func TestFreeBusyVisibilityRejects(t *testing.T) {
	for name, inner := range map[string]string{
		"unknown audience": `<x:audience>everyone</x:audience>`,
		"unknown path":     `<x:allow><d:href>/principals/ghost/</d:href></x:allow>`,
		"foreign path":     `<x:allow><d:href>/principals/user3/</d:href></x:allow>`,
		"bad address":      `<x:allow><d:href>mailto:not-an-address</d:href></x:allow>`,
		"non-principal":    `<x:allow><d:href>/calendars/user2/</d:href></x:allow>`,
		"stray element":    `<x:audience>domain</x:audience><x:colour>red</x:colour>`,
		"text value":       `nobody`,
	} {
		srv, backend, _ := availabilityServer("principals/user1")
		if body := setVisibility(t, srv, "user1", inner); !strings.Contains(body, "403") {
			t.Errorf("%s: accepted:\n%s", name, body)
		}
		if len(backend.Visibilities) != 0 {
			t.Errorf("%s: invalid setting stored", name)
		}
	}

	// No existence oracle outside the domain: an address nobody has is
	// accepted and stored exactly like a real foreign one, and a path to a
	// real foreign principal is refused exactly like a missing one (above).
	srv0, b0, _ := availabilityServer("principals/user1")
	if body := setVisibility(t, srv0, "user1", `<x:allow><d:href>mailto:nobody@nowhere.example</d:href></x:allow>`); strings.Contains(body, "403") {
		t.Errorf("unknown foreign address refused (reveals it does not exist):\n%s", body)
	}
	if got := b0.Visibilities["principals/user1"].Allowed; len(got) != 1 || got[0] != "mailto:nobody@nowhere.example" {
		t.Errorf("stored %v", got)
	}

	// A same-domain address resolves to the principal.
	srv1, b1, _ := availabilityServer("principals/user1")
	setVisibility(t, srv1, "user1", `<x:allow><d:href>mailto:user2@example.org</d:href></x:allow>`)
	if got := b1.Visibilities["principals/user1"].Allowed; len(got) != 1 || got[0] != "principals/user2" {
		t.Errorf("same-domain address stored as %v, want principals/user2", got)
	}

	// Another user cannot change (or read) user1's setting.
	srv, backend, _ := availabilityServer("principals/user2")
	setVisibility(t, srv, "user1", `<x:audience>nobody</x:audience>`)
	if len(backend.Visibilities) != 0 {
		t.Error("another user changed the visibility")
	}
	if body := propfindVisibility(t, srv, "user1"); strings.Contains(body, "</audience>") {
		t.Errorf("another user read the visibility:\n%s", body)
	}
}

type failingPolicy struct{}

func (failingPolicy) FreeBusyVisibility(context.Context, string) (acl.FreeBusyVisibility, error) {
	return acl.FreeBusyVisibility{}, errors.New("database down")
}

// A setting that cannot be read hides free-busy rather than widening it.
func TestFreeBusyVisibilityFailsClosed(t *testing.T) {
	sd := "http://sabredav.org/ns"
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user2@example.org"}},
	}}
	backend := &caldav.MockSchedulingBackend{}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/user2"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}, FreeBusyPolicy: failingPolicy{}})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{})
	if body := probeUser1(t, srv, "user2"); !strings.Contains(body, "3.7;Could not find principal") {
		t.Errorf("unreadable policy should deny:\n%s", body)
	}
}

// groupServer: user1 (example.org) owns the calendar; principals/team is a
// group whose only member is user3 (elsewhere.example); fillers adds
// principals/p0..pN-1 for the allowed-list limit.
func groupServer(t *testing.T, loggedIn string, fillers int, visibility map[string]acl.FreeBusyVisibility) *dav.Server {
	t.Helper()
	sd := "http://sabredav.org/ns"
	list := []acl.Principal{
		{URI: "principals/user1", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user1@example.org"}},
		{URI: "principals/user2", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user2@example.org"}},
		{URI: "principals/user3", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "user3@elsewhere.example"}},
		{URI: "principals/team", Props: map[xmlNM]any{{Space: sd, Local: "email-address"}: "team@example.org"}},
	}
	for i := 0; i < fillers; i++ {
		list = append(list, acl.Principal{URI: "principals/p" + strings.Repeat("x", i)})
	}
	principals := &acl.MemoryBackend{Principals: list,
		Groups: map[string][]string{"principals/team": {"principals/user3"}}}
	backend := &caldav.MockSchedulingBackend{Visibilities: visibility}
	backend.Calendars = []caldav.CalendarInfo{
		{ID: caldav.CalendarID{Calendar: "1"}, URI: "default", PrincipalURI: "principals/user1"},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(caldav.NewCalendarRoot(principals, backend, "calendars"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(loggedIn))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}, FreeBusyPolicy: backend})
	srv.AddPlugin(&caldav.Plugin{})
	srv.AddPlugin(&caldav.SchedulePlugin{})
	return srv
}

// Granting a group lets its members in.
func TestFreeBusyVisibilityGroupGrant(t *testing.T) {
	vis := map[string]acl.FreeBusyVisibility{}
	owner := groupServer(t, "principals/user1", 0, vis)
	if body := setVisibility(t, owner, "user1",
		`<x:audience>nobody</x:audience><x:allow><d:href>/principals/team/</d:href></x:allow>`); strings.Contains(body, "403") {
		t.Fatalf("PROPPATCH rejected:\n%s", body)
	}
	if got := vis["principals/user1"]; len(got.Allowed) != 1 || got.Allowed[0] != "principals/team" {
		t.Fatalf("stored = %+v", got)
	}
	if body := probeUser1(t, groupServer(t, "principals/user3", 0, vis), "user3"); !strings.Contains(body, "2.0;Success") {
		t.Errorf("group member should see free-busy:\n%s", body)
	}
	if body := probeUser1(t, groupServer(t, "principals/user2", 0, vis), "user2"); !strings.Contains(body, "3.7;") {
		t.Errorf("non-member should not see free-busy:\n%s", body)
	}
}

func TestFreeBusyVisibilityAllowedLimit(t *testing.T) {
	vis := map[string]acl.FreeBusyVisibility{}
	srv := groupServer(t, "principals/user1", 0, vis)
	var hrefs strings.Builder
	for i := 0; i < 201; i++ {
		hrefs.WriteString(fmt.Sprintf("<d:href>mailto:guest%d@elsewhere.example</d:href>", i))
	}
	if body := setVisibility(t, srv, "user1", `<x:allow>`+hrefs.String()+`</x:allow>`); !strings.Contains(body, "403") {
		t.Errorf("201 allowed principals accepted:\n%.300s", body)
	}
	if len(vis) != 0 {
		t.Error("over-long list stored")
	}
	// 200 is still fine.
	last := "<d:href>mailto:guest200@elsewhere.example</d:href>"
	if body := setVisibility(t, srv, "user1", `<x:allow>`+strings.TrimSuffix(hrefs.String(), last)+`</x:allow>`); strings.Contains(body, "403") {
		t.Errorf("200 allowed principals rejected:\n%.300s", body)
	}
	if got := len(vis["principals/user1"].Allowed); got != 200 {
		t.Errorf("stored %d allowed principals, want 200", got)
	}
}
