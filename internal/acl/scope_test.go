package acl_test

// Scoped root listings (docs/slow-queries.md, item 2): a root that lists
// only the requester's scope answers a Depth:1 PROPFIND with exactly the
// hrefs the full listing filtered by FilterChildren gave, for every kind of
// requester and on the addresses where the domain rule could diverge.

import (
	"encoding/xml"
	"regexp"
	"slices"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/dav/mock"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
)

// scopeCorpus is the principal set: two plain users of a.example, one whose
// domain differs only in case, one with two "@", one with a trailing "@",
// one of another domain, one with no email, and the admin.
var scopeCorpus = []acl.Principal{
	{URI: "principals/user1", Props: emailProps("user1@a.example")},
	{URI: "principals/user2", Props: emailProps("user2@a.example")},
	{URI: "principals/upper", Props: emailProps("upper@A.EXAMPLE")},
	{URI: "principals/twoat", Props: emailProps("two@at@a.example")},
	{URI: "principals/trailing", Props: emailProps("trailing@")},
	{URI: "principals/user3", Props: emailProps("user3@b.example")},
	{URI: "principals/noemail"},
	{URI: "principals/admin"},
}

func emailProps(address string) map[xml.Name]any {
	return map[xml.Name]any{{Space: davxml.NSSabreDAV, Local: "email-address"}: address}
}

// unscoped hides a root's ChildrenScoped, so the server lists everything and
// FilterChildren prunes: the path every root took before.
type unscoped struct{ dav.Collection }

func scopeServer(authAs string, scoped bool) *dav.Server {
	principals := &acl.MemoryBackend{Principals: scopeCorpus}
	var calendars []caldav.CalendarInfo
	for i, p := range scopeCorpus {
		calendars = append(calendars, caldav.CalendarInfo{
			ID: caldav.CalendarID{Calendar: string(rune('1' + i))}, URI: "cal", PrincipalURI: p.URI})
	}
	backend := &caldav.MockBackend{Calendars: calendars}
	root := mock.NewCollection("root")
	var principalRoot, calendarRoot dav.Collection = acl.NewPrincipalCollection(principals, "principals"),
		caldav.NewCalendarRoot(principals, backend, "calendars")
	if !scoped {
		principalRoot, calendarRoot = unscoped{principalRoot}, unscoped{calendarRoot}
	}
	root.Attach(principalRoot)
	root.Attach(calendarRoot)
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(authAs))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&caldav.Plugin{})
	return srv
}

var hrefRE = regexp.MustCompile(`<d:href>([^<]*)</d:href>`)

func listedHrefs(t *testing.T, srv *dav.Server, path string) []string {
	t.Helper()
	body := propfind(t, srv, path, "", "1").Body.String()
	var out []string
	for _, m := range hrefRE.FindAllStringSubmatch(body, -1) {
		if m[1] != path {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

func TestScopedListingEqualsFilteredListing(t *testing.T) {
	for _, requester := range []string{
		"principals/user1", "principals/upper", "principals/twoat", "principals/trailing",
		"principals/user3", "principals/noemail", "principals/admin",
	} {
		for _, path := range []string{"/principals/", "/calendars/"} {
			scoped := listedHrefs(t, scopeServer(requester, true), path)
			filtered := listedHrefs(t, scopeServer(requester, false), path)
			if !slices.Equal(scoped, filtered) {
				t.Errorf("%s listing %s: scoped %v, filtered %v", requester, path, scoped, filtered)
			}
			if len(scoped) == 0 {
				t.Errorf("%s listing %s: empty (the requester always sees itself)", requester, path)
			}
		}
	}
}

// The scope itself: user1 and the case-different and two-"@" users share
// a.example; the trailing-"@" user and the no-email user have no domain; an
// admin gets no scope at all.
func TestChildScopeFollowsEmailDomainRule(t *testing.T) {
	for _, tc := range []struct {
		requester string
		want      []string // hrefs under /principals/, "" meaning no scope
	}{
		{"principals/user1", []string{"/principals/twoat/", "/principals/upper/", "/principals/user1/", "/principals/user2/"}},
		{"principals/trailing", []string{"/principals/trailing/"}},
		{"principals/noemail", []string{"/principals/noemail/"}},
	} {
		if got := listedHrefs(t, scopeServer(tc.requester, true), "/principals/"); !slices.Equal(got, tc.want) {
			t.Errorf("%s sees %v, want %v", tc.requester, got, tc.want)
		}
	}
	if got := listedHrefs(t, scopeServer("principals/admin", true), "/principals/"); len(got) != len(scopeCorpus) {
		t.Errorf("admin sees %d principals, want all %d: %v", len(got), len(scopeCorpus), got)
	}
}

func TestEmailDomain(t *testing.T) {
	for email, want := range map[string]string{
		"user@a.example": "a.example", "User@A.Example": "a.example",
		"two@at@a.example": "a.example", "trailing@": "", "noat": "", "": "", "@only": "only",
	} {
		if got := acl.EmailDomain(email); got != want {
			t.Errorf("EmailDomain(%q) = %q, want %q", email, got, want)
		}
	}
}
