package admin

// Error-path and branch coverage tests: storage failures surfacing as 500s,
// missing/malformed query params as 400s, empty lookups as 404s, write-gate
// refusals, malformed digests, and the template helper branches. Follows the
// established fixtures: fakeStore/fakeWrites plus thin failing wrappers.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"calendsd/internal/acl"
	"calendsd/internal/auth"
	"calendsd/internal/caldav"
	"calendsd/internal/carddav"
	"calendsd/internal/dav"
	"calendsd/internal/dav/mock"
	"calendsd/internal/storage"
)

var errBoom = errors.New("boom")

// failStore fails exactly the Store method named by fail, delegating the
// rest to the fakeStore fixture.
type failStore struct {
	fakeStore
	fail string
}

func (s failStore) Counts(ctx context.Context) (*storage.AdminCounts, error) {
	if s.fail == "Counts" {
		return nil, errBoom
	}
	return s.fakeStore.Counts(ctx)
}

func (s failStore) RecentChanges(ctx context.Context, limit int) ([]storage.AdminChange, error) {
	if s.fail == "RecentChanges" {
		return nil, errBoom
	}
	return s.fakeStore.RecentChanges(ctx, limit)
}

func (s failStore) AuthDiagnostics(ctx context.Context) (*storage.AdminAuthDiagnostics, error) {
	if s.fail == "AuthDiagnostics" {
		return nil, errBoom
	}
	return s.fakeStore.AuthDiagnostics(ctx)
}

func (s failStore) UserForPrincipal(ctx context.Context, principalURI string) (*storage.AdminUserRow, error) {
	if s.fail == "UserForPrincipal" {
		return nil, errBoom
	}
	return s.fakeStore.UserForPrincipal(ctx, principalURI)
}

func (s failStore) LookupCalendar(ctx context.Context, principalURI, uri string) (string, error) {
	if s.fail == "LookupCalendar" {
		return "", errBoom
	}
	return s.fakeStore.LookupCalendar(ctx, principalURI, uri)
}

func (s failStore) CalendarInstances(ctx context.Context, calendarID string) ([]storage.AdminCalendarInstance, error) {
	if s.fail == "CalendarInstances" {
		return nil, errBoom
	}
	return s.fakeStore.CalendarInstances(ctx, calendarID)
}

func (s failStore) CalendarMeta(ctx context.Context, calendarID string) (*storage.AdminCalendarMeta, error) {
	if s.fail == "CalendarMeta" {
		return nil, errBoom
	}
	return s.fakeStore.CalendarMeta(ctx, calendarID)
}

func (s failStore) PropertyRows(ctx context.Context, path string) ([]storage.AdminPropRow, error) {
	if s.fail == "PropertyRows" {
		return nil, errBoom
	}
	return s.fakeStore.PropertyRows(ctx, path)
}

func (s failStore) AddressBook(ctx context.Context, id string) (*storage.AdminAddressBook, error) {
	if s.fail == "AddressBook" {
		return nil, errBoom
	}
	return s.fakeStore.AddressBook(ctx, id)
}

func (s failStore) LookupAddressBook(ctx context.Context, principalURI, uri string) (string, error) {
	if s.fail == "LookupAddressBook" {
		return "", errBoom
	}
	return s.fakeStore.LookupAddressBook(ctx, principalURI, uri)
}

func (s failStore) Objects(ctx context.Context, q storage.AdminObjectQuery) ([]storage.AdminObjectRow, bool, error) {
	if s.fail == "Objects" {
		return nil, false, errBoom
	}
	return s.fakeStore.Objects(ctx, q)
}

func (s failStore) Object(ctx context.Context, calendarID, uri string) (*storage.AdminObjectDetail, error) {
	if s.fail == "Object" {
		return nil, errBoom
	}
	return s.fakeStore.Object(ctx, calendarID, uri)
}

func (s failStore) CardsSearch(ctx context.Context, bookID, match string, inData bool, limit int) ([]storage.AdminCardRow, bool, error) {
	if s.fail == "CardsSearch" {
		return nil, false, errBoom
	}
	return s.fakeStore.CardsSearch(ctx, bookID, match, inData, limit)
}

func (s failStore) Card(ctx context.Context, bookID, uri string) (*storage.AdminCardDetail, error) {
	if s.fail == "Card" {
		return nil, errBoom
	}
	return s.fakeStore.Card(ctx, bookID, uri)
}

func (s failStore) ChangeLog(ctx context.Context, kind, collectionID string, limit int) ([]storage.AdminChange, error) {
	if s.fail == "ChangeLog" {
		return nil, errBoom
	}
	return s.fakeStore.ChangeLog(ctx, kind, collectionID, limit)
}

func (s failStore) PendingShareInvites(ctx context.Context, principalURI string) ([]storage.AdminCalendarInstance, error) {
	if s.fail == "PendingShareInvites" {
		return nil, errBoom
	}
	return s.fakeStore.PendingShareInvites(ctx, principalURI)
}

func (s failStore) Doctor(ctx context.Context, scanCap int) (*storage.AdminDoctorReport, error) {
	if s.fail == "Doctor" {
		return nil, errBoom
	}
	return s.fakeStore.Doctor(ctx, scanCap)
}

// TestStoreErrorsSurfaceAs500 walks every data handler with each of its
// Store dependencies failing in turn.
func TestStoreErrorsSurfaceAs500(t *testing.T) {
	cases := []struct{ fail, path string }{
		{"Counts", "/api/dashboard"},
		{"RecentChanges", "/api/dashboard"},
		{"AuthDiagnostics", "/api/dashboard"},
		{"AuthDiagnostics", "/api/principals"},
		{"UserForPrincipal", "/api/principal?path=principals/alice"},
		{"LookupCalendar", "/api/calendar?principal=principals/alice&uri=default"},
		{"CalendarMeta", "/api/calendar?id=7"},
		{"CalendarInstances", "/api/calendar?id=7"},
		{"PropertyRows", "/api/calendar?id=7"},
		{"AddressBook", "/api/addressbook?id=3"},
		{"LookupAddressBook", "/api/addressbook?principal=principals/alice&uri=contacts"},
		{"PropertyRows", "/api/addressbook?id=3"},
		{"Objects", "/api/objects?calendar=7"},
		{"LookupCalendar", "/api/objects?principal=principals/alice&uri=default"},
		{"Object", "/api/object?calendar=7&uri=a.ics"},
		{"CardsSearch", "/api/cards?book=3"},
		{"Card", "/api/card?book=3&uri=card1.vcf"},
		{"ChangeLog", "/api/sync?id=7"},
		{"PendingShareInvites", "/api/scheduling?principal=principals/bob"},
		{"Doctor", "/api/doctor"},
	}
	for _, c := range cases {
		a := testApp()
		a.Store = failStore{fail: c.fail}
		if w := get(t, a.Handler(), c.path); w.Code != http.StatusInternalServerError {
			t.Errorf("GET %s with %s failing = %d, want 500", c.path, c.fail, w.Code)
		}
	}
}

// failPrincipals fails one acl.Backend method, delegating the rest.
type failPrincipals struct {
	acl.Backend
	fail string
}

func (f failPrincipals) PrincipalsByPrefix(ctx context.Context, prefix string) ([]acl.Principal, error) {
	if f.fail == "PrincipalsByPrefix" {
		return nil, errBoom
	}
	return f.Backend.PrincipalsByPrefix(context.Background(), prefix)
}

func (f failPrincipals) PrincipalByPath(ctx context.Context, path string) (*acl.Principal, error) {
	if f.fail == "PrincipalByPath" {
		return nil, errBoom
	}
	return f.Backend.PrincipalByPath(context.Background(), path)
}

func (f failPrincipals) GroupMembership(ctx context.Context, principal string) ([]string, error) {
	if f.fail == "GroupMembership" {
		return nil, errBoom
	}
	return f.Backend.GroupMembership(context.Background(), principal)
}

func (f failPrincipals) GroupMemberSet(ctx context.Context, principal string) ([]string, error) {
	if f.fail == "GroupMemberSet-read" && strings.HasSuffix(principal, "-read") {
		return nil, errBoom
	}
	if f.fail == "GroupMemberSet-write" && strings.HasSuffix(principal, "-write") {
		return nil, errBoom
	}
	return f.Backend.GroupMemberSet(context.Background(), principal)
}

func (f failPrincipals) SetGroupMemberSet(ctx context.Context, principal string, members []string) error {
	if f.fail == "SetGroupMemberSet" {
		return errBoom
	}
	return f.Backend.SetGroupMemberSet(context.Background(), principal, members)
}

// failCalendars fails the calendar-home listing; everything else delegates.
type failCalendars struct{ caldav.Backend }

func (failCalendars) CalendarsForUser(context.Context, string) ([]caldav.CalendarInfo, error) {
	return nil, errBoom
}

// failBooks fails the address-book-home listing.
type failBooks struct{ carddav.Backend }

func (failBooks) AddressBooksForUser(context.Context, string) ([]carddav.AddressBookInfo, error) {
	return nil, errBoom
}

func TestPrincipalBackendErrorsSurfaceAs500(t *testing.T) {
	for _, fail := range []string{"PrincipalsByPrefix", "PrincipalByPath",
		"GroupMembership", "GroupMemberSet-read", "GroupMemberSet-write"} {
		a := testApp()
		a.Principals = failPrincipals{Backend: a.Principals, fail: fail}
		path := "/api/principal?path=principals/alice"
		if fail == "PrincipalsByPrefix" {
			path = "/api/principals"
		}
		if w := get(t, a.Handler(), path); w.Code != http.StatusInternalServerError {
			t.Errorf("GET %s with %s failing = %d, want 500", path, fail, w.Code)
		}
	}

	a := testApp()
	a.Calendars = failCalendars{Backend: a.Calendars}
	if w := get(t, a.Handler(), "/api/principal?path=principals/alice"); w.Code != http.StatusInternalServerError {
		t.Errorf("CalendarsForUser failing = %d, want 500", w.Code)
	}

	a = testApp()
	a.AddressBooks = failBooks{Backend: a.AddressBooks}
	if w := get(t, a.Handler(), "/api/principal?path=principals/alice"); w.Code != http.StatusInternalServerError {
		t.Errorf("AddressBooksForUser failing = %d, want 500", w.Code)
	}
}

// TestPrincipalGroupClassification covers the proxy-write and plain-group
// arms of the delegation rendering plus splitProxy's non-proxy shapes.
func TestPrincipalGroupClassification(t *testing.T) {
	a := testApp()
	mb := a.Principals.(*acl.MemoryBackend)
	// A slashless group and a slashed non-proxy group land in Groups verbatim.
	mb.Groups["staff"] = []string{"principals/bob"}
	mb.Groups["groups/team"] = []string{"principals/bob"}
	d := getJSON[PrincipalDetail](t, a.Handler(), "/api/principal?path=principals/bob")
	if len(d.SeesCalendarsOfWrite) != 1 || d.SeesCalendarsOfWrite[0] != "principals/alice" {
		t.Fatalf("seesCalendarsOfWrite = %v, want [principals/alice]", d.SeesCalendarsOfWrite)
	}
	groups := strings.Join(d.Groups, " ")
	if !strings.Contains(groups, "staff") || !strings.Contains(groups, "groups/team") {
		t.Fatalf("plain groups = %v, want staff and groups/team", d.Groups)
	}
}

// weirdStore serves malformed and stale rows on top of the fixture.
type weirdStore struct{ fakeStore }

func (s weirdStore) Object(ctx context.Context, calendarID, uri string) (*storage.AdminObjectDetail, error) {
	switch uri {
	case "garbage.ics":
		return &storage.AdminObjectDetail{Row: storage.AdminObjectRow{URI: uri},
			Data: []byte("this is not ical")}, nil
	case "allstale.ics":
		row := storedRow("allstale.ics", testICS)
		row.Size = 1
		row.ComponentType = "VTODO"
		row.UID = "someone-else"
		row.LastOccurrence = nil
		return &storage.AdminObjectDetail{Row: row, Data: []byte(testICS)}, nil
	}
	return s.fakeStore.Object(ctx, calendarID, uri)
}

func (s weirdStore) Card(ctx context.Context, bookID, uri string) (*storage.AdminCardDetail, error) {
	if uri == "stale.vcf" {
		return &storage.AdminCardDetail{
			Row:  storage.AdminCardRow{URI: uri, ETag: "bogus", Size: 999},
			Data: []byte(testVCF)}, nil
	}
	return s.fakeStore.Card(ctx, bookID, uri)
}

func (s weirdStore) UserForPrincipal(ctx context.Context, principalURI string) (*storage.AdminUserRow, error) {
	switch principalURI {
	case "principals/bob": // too short to be an md5 hex
		return &storage.AdminUserRow{Username: "bob", DigestA1: "notahash"}, nil
	case "principals/carol": // right length, not hex
		return &storage.AdminUserRow{Username: "carol",
			DigestA1: strings.Repeat("Z", 32)}, nil
	}
	return s.fakeStore.UserForPrincipal(ctx, principalURI)
}

func TestObjectParseErrorAndAllStaleColumns(t *testing.T) {
	a := testApp()
	a.Store = weirdStore{}
	h := a.Handler()

	g := getJSON[ObjectDetail](t, h, "/api/object?calendar=7&uri=garbage.ics")
	if g.ParseError == "" || g.Derived != nil {
		t.Fatalf("garbage object = %+v, want parse error and no derived fields", g)
	}

	s := getJSON[ObjectDetail](t, h, "/api/object?calendar=7&uri=allstale.ics")
	want := map[string]bool{"size": true, "componenttype": true, "uid": true, "lastoccurence": true}
	if len(s.Diffs) != len(want) {
		t.Fatalf("allstale diffs = %v, want %v", s.Diffs, want)
	}
	for _, d := range s.Diffs {
		if !want[d] {
			t.Fatalf("allstale diffs = %v, want %v", s.Diffs, want)
		}
	}
}

func TestCardStaleColumns(t *testing.T) {
	a := testApp()
	a.Store = weirdStore{}
	d := getJSON[CardDetail](t, a.Handler(), "/api/card?book=3&uri=stale.vcf")
	if len(d.Diffs) != 2 {
		t.Fatalf("stale card diffs = %v, want etag+size", d.Diffs)
	}
}

func TestMalformedDigest(t *testing.T) {
	a := testApp()
	a.Store = weirdStore{}
	h := a.Handler()

	// The principal page keeps a malformed digest visible instead of hiding it.
	d := getJSON[PrincipalDetail](t, h, "/api/principal?path=principals/bob")
	if d.DigestWellFormed || d.User == nil || d.User.DigestA1 != "notahash" {
		t.Fatalf("malformed digest principal = wellformed=%t user=%+v", d.DigestWellFormed, d.User)
	}

	// checkCredentials reports it too — both the short and the non-hex shape.
	for _, user := range []string{"bob", "carol"} {
		w := postForm(t, h, "/api/check-credentials",
			url.Values{"username": {user}, "password": {"x"}}, true)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "not a 32-char md5 hex") {
			t.Fatalf("check-credentials %s = %d: %s", user, w.Code, w.Body.String())
		}
	}
}

func TestCheckCredentialsParamAndStoreErrors(t *testing.T) {
	a := testApp()
	h := a.Handler()
	if w := postForm(t, h, "/api/check-credentials", url.Values{}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("missing username = %d, want 400", w.Code)
	}
	a.Store = failStore{fail: "UserForPrincipal"}
	if w := postForm(t, a.Handler(), "/api/check-credentials",
		url.Values{"username": {"alice"}, "password": {"x"}}, true); w.Code != http.StatusInternalServerError {
		t.Fatalf("store error = %d, want 500", w.Code)
	}
}

// TestInspectorParamValidation covers the missing-param 400s and empty-lookup
// 404s of the calendar/addressbook/object/card pages.
func TestInspectorParamValidation(t *testing.T) {
	h := testApp().Handler()
	for path, want := range map[string]int{
		"/api/calendar": http.StatusBadRequest,
		"/api/calendar?principal=principals/alice&uri=nope": http.StatusNotFound,
		"/api/addressbook": http.StatusBadRequest,
		"/api/addressbook?principal=principals/alice&uri=nope": http.StatusNotFound,
		"/api/addressbook?id=999":                              http.StatusNotFound,
		"/api/objects":                                         http.StatusBadRequest,
		"/api/objects?principal=principals/alice&uri=nope":     http.StatusNotFound,
		"/api/objects?calendar=7&start=bogus":                  http.StatusBadRequest,
		"/api/objects?calendar=7&end=bogus":                    http.StatusBadRequest,
		"/api/objects?calendar=7&limit=x":                      http.StatusBadRequest,
		"/api/object?calendar=7":                               http.StatusBadRequest,
		"/api/object?calendar=7&uri=nope.ics":                  http.StatusNotFound,
		"/api/cards":                                           http.StatusBadRequest,
		"/api/cards?book=3&limit=x":                            http.StatusBadRequest,
		"/api/card?book=3":                                     http.StatusBadRequest,
		"/api/card?book=3&uri=nope.vcf":                        http.StatusNotFound,
	} {
		if w := get(t, h, path); w.Code != want {
			t.Errorf("GET %s = %d, want %d", path, w.Code, want)
		}
	}
}

// TestLookupParamForms covers the ?principal=&uri= resolution paths and the
// time/limit parameter formats.
func TestLookupParamForms(t *testing.T) {
	h := testApp().Handler()

	b := getJSON[AddressBookDetail](t, h, "/api/addressbook?principal=principals/alice&uri=contacts")
	if b.Book == nil || b.Book.ID != "3" {
		t.Fatalf("addressbook by principal/uri = %+v", b)
	}

	o := getJSON[ObjectsData](t, h, "/api/objects?principal=principals/alice&uri=default")
	if o.CalendarID != "7" {
		t.Fatalf("objects by principal/uri = %+v", o)
	}

	// Unix seconds, RFC 3339, and an explicit limit all parse.
	o = getJSON[ObjectsData](t, h,
		"/api/objects?calendar=7&start=1750000000&end=2026-06-01T00:00:00Z&limit=5")
	if o.CalendarID != "7" {
		t.Fatalf("objects with time params = %+v", o)
	}

	c := getJSON[CardsData](t, h, "/api/cards?book=3&limit=2")
	if len(c.Cards) != 1 {
		t.Fatalf("cards with limit = %+v", c)
	}
}

// --- debug.go branches ---

func TestSchedulingParamsAndUnavailable(t *testing.T) {
	// Missing principal.
	if w := get(t, testApp().Handler(), "/api/scheduling"); w.Code != http.StatusBadRequest {
		t.Fatalf("missing principal = %d, want 400", w.Code)
	}
	// No scheduling backend wired: the page says so.
	d := getJSON[SchedulingData](t, testApp().Handler(), "/api/scheduling?principal=principals/bob")
	if !d.InboxUnavailable {
		t.Fatalf("nil scheduling backend: %+v, want InboxUnavailable", d)
	}
}

// failScheduling fails the inbox listing.
type failScheduling struct{ fakeScheduling }

func (failScheduling) SchedulingObjects(context.Context, string) ([]caldav.SchedulingObjectInfo, error) {
	return nil, errBoom
}

func TestSchedulingBackendError(t *testing.T) {
	a := testApp()
	a.Scheduling = failScheduling{}
	if w := get(t, a.Handler(), "/api/scheduling?principal=principals/bob"); w.Code != http.StatusInternalServerError {
		t.Fatalf("scheduling backend error = %d, want 500", w.Code)
	}
}

func TestParseInboxItemBranches(t *testing.T) {
	// Unparsable payload keeps the raw data and reports the error.
	bad := parseInboxItem(caldav.SchedulingObjectInfo{URI: "bad.ics",
		CalendarData: []byte("this is not ical")})
	if bad.ParseError == "" || bad.Data == "" {
		t.Fatalf("bad item = %+v, want parse error with raw data", bad)
	}

	// VTIMEZONE is skipped; a PARTSTAT-less attendee defaults to NEEDS-ACTION;
	// a duplicate attendee is folded.
	itip := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//test//test//EN",
		"METHOD:REPLY",
		"BEGIN:VTIMEZONE",
		"TZID:UTC",
		"BEGIN:STANDARD",
		"DTSTART:19700101T000000",
		"TZOFFSETFROM:+0000",
		"TZOFFSETTO:+0000",
		"END:STANDARD",
		"END:VTIMEZONE",
		"BEGIN:VEVENT",
		"UID:evt-2",
		"DTSTAMP:20260101T000000Z",
		"DTSTART:20260401T090000Z",
		"ATTENDEE:mailto:bob@example.org",
		"ATTENDEE:mailto:bob@example.org",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
	item := parseInboxItem(caldav.SchedulingObjectInfo{URI: "reply.ics",
		CalendarData: []byte(itip)})
	if item.ParseError != "" {
		t.Fatalf("itip parse error: %s", item.ParseError)
	}
	if item.Method != "REPLY" || item.UID != "evt-2" {
		t.Fatalf("item = %+v", item)
	}
	if len(item.Attendees) != 1 || item.Attendees[0].PartStat != "NEEDS-ACTION" {
		t.Fatalf("attendees = %+v, want one deduped NEEDS-ACTION attendee", item.Attendees)
	}
}

// errSyncCalendars fails the sync preview.
type errSyncCalendars struct{ caldav.Backend }

func (errSyncCalendars) ChangesForCalendar(context.Context, caldav.CalendarID, string, int, int) (*dav.ChangeSet, error) {
	return nil, errBoom
}

func TestSyncParamsAndBranches(t *testing.T) {
	h := testApp().Handler()
	for path, want := range map[string]int{
		"/api/sync?kind=journal&id=7": http.StatusBadRequest,
		"/api/sync?kind=calendar":     http.StatusBadRequest,
		"/api/sync?id=7&limit=x":      http.StatusBadRequest,
	} {
		if w := get(t, h, path); w.Code != want {
			t.Errorf("GET %s = %d, want %d", path, w.Code, want)
		}
	}

	// Omitted kind defaults to calendar; the plain mock has no sync data.
	d := getJSON[SyncData](t, h, "/api/sync?id=7&limit=10")
	if d.Kind != "calendar" || d.Preview != nil || d.PreviewNote == "" {
		t.Fatalf("default-kind sync = %+v", d)
	}

	// The addressbook arm.
	b := getJSON[SyncData](t, h, "/api/sync?kind=addressbook&id=3")
	if b.Kind != "addressbook" || b.Preview != nil {
		t.Fatalf("addressbook sync = %+v", b)
	}

	// Backend failure surfaces as 500.
	a := testApp()
	a.Calendars = errSyncCalendars{Backend: a.Calendars}
	if w := get(t, a.Handler(), "/api/sync?kind=calendar&id=7"); w.Code != http.StatusInternalServerError {
		t.Fatalf("sync backend error = %d, want 500", w.Code)
	}
}

func TestDoctorScanCap(t *testing.T) {
	h := testAppB().Handler()
	d := getJSON[DoctorData](t, h, "/api/doctor?scancap=5")
	if len(d.Report.Findings) != 1 {
		t.Fatalf("doctor with scancap = %+v", d.Report)
	}
	if w := get(t, h, "/api/doctor?scancap=x"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad scancap = %d, want 400", w.Code)
	}
}

// --- simulate.go branches ---

func TestSimulateParamAndAvailabilityBranches(t *testing.T) {
	// One of the two params missing.
	if w := get(t, testAppB().Handler(), "/api/simulate?principal=principals/alice"); w.Code != http.StatusBadRequest {
		t.Fatalf("half params = %d, want 400", w.Code)
	}
	// No simulator wired.
	d := getJSON[SimulateData](t, testApp().Handler(),
		"/api/simulate?principal=principals/alice&path=/calendars/alice/default")
	if !d.Unavailable || d.Requested {
		t.Fatalf("nil simulator = %+v, want Unavailable", d)
	}
	// A path without the leading slash is normalized.
	s := getJSON[SimulateData](t, testAppB().Handler(),
		"/api/simulate?principal=principals/alice&path=calendars/alice/default")
	if s.Path != "/calendars/alice/default" || s.Status != http.StatusMultiStatus {
		t.Fatalf("slashless path = %+v", s)
	}
	// A nonexistent node yields the not-found ACL note, not an error.
	n := getJSON[SimulateData](t, testAppB().Handler(),
		"/api/simulate?principal=principals/alice&path=/calendars/alice/nope")
	if !strings.Contains(n.ACLNote, "node not found") {
		t.Fatalf("missing node aclNote = %q", n.ACLNote)
	}
}

func TestSimulateWithoutACLPlugin(t *testing.T) {
	a := testAppB()
	a.NewSimulator = func(principal string) *dav.Server {
		srv := dav.NewServer("/", mock.NewCollection("root"))
		srv.AddPlugin(auth.NewLoggedInPlugin(principal))
		return srv
	}
	d := getJSON[SimulateData](t, a.Handler(), "/api/simulate?principal=principals/alice&path=/")
	if !strings.Contains(d.ACLNote, "acl plugin not in the composition") {
		t.Fatalf("aclNote = %q", d.ACLNote)
	}
}

func TestParsePrivilegesBadXML(t *testing.T) {
	if _, err := parsePrivileges([]byte("<not-xml")); err == nil {
		t.Fatal("malformed multistatus accepted; want an error")
	}
}

// --- templates.go branches ---

// helperStore serves rows whose enum values hit the template helper arms
// the standard fixture misses (access 3/unknown, op add/unknown, every
// invite status).
type helperStore struct{ fakeStore }

func (s helperStore) CalendarInstances(ctx context.Context, calendarID string) ([]storage.AdminCalendarInstance, error) {
	return []storage.AdminCalendarInstance{
		{InstanceID: "20", CalendarID: "7", PrincipalURI: "principals/alice",
			URI: "default", Access: 3, InviteStatus: 1},
		{InstanceID: "21", CalendarID: "7", PrincipalURI: "principals/bob",
			URI: "b", Access: 99, InviteStatus: 3},
		{InstanceID: "22", CalendarID: "7", PrincipalURI: "principals/carol",
			URI: "c", Access: 1, InviteStatus: 4},
		{InstanceID: "23", CalendarID: "7", PrincipalURI: "principals/dave",
			URI: "d", Access: 1, InviteStatus: 99},
	}, nil
}

func (s helperStore) RecentChanges(ctx context.Context, limit int) ([]storage.AdminChange, error) {
	return []storage.AdminChange{
		{Kind: "calendar", CollectionID: "7", CollectionURI: "default",
			PrincipalURI: "principals/alice", ObjectURI: "new.ics", SyncToken: 1,
			Operation: 1, LastModified: i64(1750000000)},
		{Kind: "calendar", CollectionID: "7", CollectionURI: "default",
			PrincipalURI: "principals/alice", ObjectURI: "odd.ics", SyncToken: 2,
			Operation: 99, LastModified: i64(1750000001)},
	}, nil
}

func TestTemplateHelperBranches(t *testing.T) {
	a := testApp()
	a.Store = helperStore{}
	h := a.Handler()

	cal := get(t, h, "/calendar?id=7")
	if cal.Code != http.StatusOK {
		t.Fatalf("calendar page = %d: %s", cal.Code, cal.Body.String())
	}
	for _, marker := range []string{"read-write", "no response", "declined", "invalid", "?"} {
		if !strings.Contains(cal.Body.String(), marker) {
			t.Errorf("calendar page missing helper output %q", marker)
		}
	}

	dash := get(t, h, "/")
	if dash.Code != http.StatusOK {
		t.Fatalf("dashboard = %d: %s", dash.Code, dash.Body.String())
	}
	for _, marker := range []string{"add", "?"} {
		if !strings.Contains(dash.Body.String(), marker) {
			t.Errorf("dashboard missing helper output %q", marker)
		}
	}
}

func TestRenderPageErrorSurfaces(t *testing.T) {
	w := httptest.NewRecorder()
	renderPage(w, "no-such-template", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unknown template = %d, want 500", w.Code)
	}
}

// --- auth.go branches ---

func TestLoadHTPasswdOpenAndScanErrors(t *testing.T) {
	if _, err := LoadHTPasswd("/nonexistent/htpasswd"); err == nil {
		t.Fatal("missing file accepted; want an error")
	}
	// A line beyond bufio.Scanner's token limit fails via sc.Err().
	long := "root:" + strings.Repeat("a", 128*1024) + "\n"
	if _, err := LoadHTPasswd(writeHTPasswd(t, long)); err == nil {
		t.Fatal("oversized line accepted; want an error")
	}
}

func TestActorFromContext(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, "root"))
	if got := actor(req); got != "root" {
		t.Fatalf("actor = %q, want root", got)
	}
	if got := actor(httptest.NewRequest("GET", "/", nil)); got != "unknown" {
		t.Fatalf("actor without context = %q, want unknown", got)
	}
}

// --- writes.go branches ---

// failWrites fails exactly the Writes method named by fail.
type failWrites struct {
	fakeWrites
	fail string
}

func (f *failWrites) CreateUser(ctx context.Context, username, digestA1, email, displayName string) error {
	if f.fail == "CreateUser" {
		return errBoom
	}
	return f.fakeWrites.CreateUser(ctx, username, digestA1, email, displayName)
}

func (f *failWrites) SetPassword(ctx context.Context, username, digestA1 string) error {
	if f.fail == "SetPassword" {
		return errBoom
	}
	return f.fakeWrites.SetPassword(ctx, username, digestA1)
}

func (f *failWrites) DeleteUser(ctx context.Context, username string) error {
	if f.fail == "DeleteUser" {
		return errBoom
	}
	return f.fakeWrites.DeleteUser(ctx, username)
}

func (f *failWrites) ScheduleDelete(ctx context.Context, username string, days int, reason string) error {
	if f.fail == "ScheduleDelete" {
		return errBoom
	}
	return f.fakeWrites.ScheduleDelete(ctx, username, days, reason)
}

func (f *failWrites) CancelDelete(ctx context.Context, username string) error {
	if f.fail == "CancelDelete" {
		return errBoom
	}
	return f.fakeWrites.CancelDelete(ctx, username)
}

func (f *failWrites) Deletions(ctx context.Context) ([]storage.DeletedPrincipal, error) {
	if f.fail == "Deletions" {
		return nil, errBoom
	}
	return f.fakeWrites.Deletions(ctx)
}

func (f *failWrites) DeleteOrphans(ctx context.Context, check string) (int64, error) {
	if f.fail == "DeleteOrphans" {
		return 0, errBoom
	}
	return f.fakeWrites.DeleteOrphans(ctx, check)
}

func TestWriteCarveOutErrors(t *testing.T) {
	cases := []struct {
		fail, path string
		form       url.Values
		want       int
	}{
		// A generic storage failure is a 500 with a generic body (audit
		// 6.17); only a missing principal (404) or an operator guard (409)
		// keeps a specific status, see TestWriteErrorMapping.
		{"CreateUser", "/api/create-user",
			url.Values{"username": {"x"}, "password": {"y"}}, http.StatusInternalServerError},
		{"SetPassword", "/api/set-password",
			url.Values{"username": {"x"}, "password": {"y"}}, http.StatusInternalServerError},
		{"DeleteUser", "/api/delete-user",
			url.Values{"username": {"x"}}, http.StatusInternalServerError},
		{"ScheduleDelete", "/api/schedule-delete",
			url.Values{"username": {"x"}}, http.StatusInternalServerError},
		{"CancelDelete", "/api/restore-user",
			url.Values{"username": {"x"}}, http.StatusInternalServerError},
		{"DeleteOrphans", "/api/delete-orphans",
			url.Values{"check": {"orphaned-instance"}}, http.StatusBadRequest},
	}
	for _, c := range cases {
		a := testAppB()
		a.Writes = &failWrites{fail: c.fail}
		a.ReadOnly = false
		if w := postForm(t, a.Handler(), c.path, c.form, true); w.Code != c.want {
			t.Errorf("POST %s with %s failing = %d, want %d", c.path, c.fail, w.Code, c.want)
		}
	}

	// Deletions failing breaks the principal page (it lists pending deletes).
	a := testApp()
	a.Writes = &failWrites{fail: "Deletions"}
	if w := get(t, a.Handler(), "/api/principal?path=principals/alice"); w.Code != http.StatusInternalServerError {
		t.Errorf("principal page with Deletions failing = %d, want 500", w.Code)
	}
}

func TestWriteParamValidation(t *testing.T) {
	a := testAppB()
	a.Writes = &fakeWrites{}
	a.ReadOnly = false
	h := a.Handler()
	for path, form := range map[string]url.Values{
		"/api/set-password": {"username": {"x"}},
		"/api/restore-user": {},
		"/api/proppatch":    {"path": {"/calendars/alice/default"}},
	} {
		if w := postForm(t, h, path, form, true); w.Code != http.StatusBadRequest {
			t.Errorf("POST %s with missing fields = %d, want 400", path, w.Code)
		}
	}
}

// TestAllWriteEndpointsGated hits every POST endpoint in each refused state:
// no CSRF header, read-only mode, and missing wiring.
func TestAllWriteEndpointsGated(t *testing.T) {
	endpoints := []string{
		"/api/create-user", "/api/set-password", "/api/delete-user",
		"/api/schedule-delete", "/api/restore-user", "/api/set-delegation",
		"/api/proppatch", "/api/provision", "/api/delete-orphans",
	}
	form := url.Values{"username": {"x"}, "password": {"y"},
		"principal": {"principals/alice"}, "kind": {"read"},
		"path": {"/x"}, "name": {"{DAV:}displayname"}}

	// Missing CSRF header refuses regardless of everything else.
	a := testAppB()
	a.Writes = &fakeWrites{}
	a.ReadOnly = false
	for _, ep := range endpoints {
		if w := postForm(t, a.Handler(), ep, form, false); w.Code != http.StatusForbidden {
			t.Errorf("POST %s without CSRF header = %d, want 403", ep, w.Code)
		}
	}

	// Read-only refuses with the header present.
	a.ReadOnly = true
	for _, ep := range endpoints {
		if w := postForm(t, a.Handler(), ep, form, true); w.Code != http.StatusForbidden {
			t.Errorf("POST %s in read-only = %d, want 403", ep, w.Code)
		}
	}

	// proppatch and provision also refuse when their factories are nil.
	b := testApp() // no NewSimulator/NewProvisioner
	b.Writes = &fakeWrites{}
	b.ReadOnly = false
	for _, ep := range []string{"/api/proppatch", "/api/provision"} {
		if w := postForm(t, b.Handler(), ep, form, true); w.Code != http.StatusForbidden {
			t.Errorf("POST %s without factory = %d, want 403", ep, w.Code)
		}
	}
}

func TestSetDelegationBackendErrors(t *testing.T) {
	for _, fail := range []string{"PrincipalByPath", "SetGroupMemberSet"} {
		a := testAppB()
		a.Writes = &fakeWrites{}
		a.ReadOnly = false
		a.Principals = failPrincipals{Backend: a.Principals, fail: fail}
		w := postForm(t, a.Handler(), "/api/set-delegation",
			url.Values{"principal": {"principals/alice"}, "kind": {"read"},
				"members": {"principals/bob"}}, true)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("set-delegation with %s failing = %d, want 500", fail, w.Code)
		}
	}
}

func TestPropPatchRemoveSlashlessAndRejected(t *testing.T) {
	a := testAppB()
	a.Writes = &fakeWrites{}
	a.ReadOnly = false
	factory, backend := newMockSimulatorEnv()
	a.NewSimulator = factory
	h := a.Handler()

	// A slashless path is normalized; the set still lands in the backend.
	w := postForm(t, h, "/api/proppatch",
		url.Values{"path": {"calendars/alice/default"},
			"name": {"{DAV:}displayname"}, "value": {"Slashless"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("slashless proppatch = %d: %s", w.Code, w.Body.String())
	}
	cals, _ := backend.CalendarsForUser(t.Context(), "principals/alice")
	if got := cals[0].Props[xmlName{Space: "DAV:", Local: "displayname"}]; got != "Slashless" {
		t.Fatalf("displayname = %v, want Slashless", got)
	}

	// remove=1 issues a d:remove instead of a d:set. The chain answers a
	// remove with "204 No Content" propstats — a success like any other
	// 2xx; the property must be gone from the backend afterwards.
	w = postForm(t, h, "/api/proppatch",
		url.Values{"path": {"/calendars/alice/default"},
			"name": {"{DAV:}displayname"}, "remove": {"1"}}, true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "property removed") {
		t.Fatalf("remove proppatch = %d: %s", w.Code, w.Body.String())
	}
	cals, _ = backend.CalendarsForUser(t.Context(), "principals/alice")
	if got, present := cals[0].Props[xmlName{Space: "DAV:", Local: "displayname"}]; present {
		t.Fatalf("displayname still present after remove: %v", got)
	}

	// A PROPPATCH the chain refuses (nonexistent path) surfaces as 502.
	w = postForm(t, h, "/api/proppatch",
		url.Values{"path": {"/calendars/alice/nope"},
			"name": {"{DAV:}displayname"}, "value": {"x"}}, true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("proppatch on missing path = %d, want 502: %s", w.Code, w.Body.String())
	}
}

func TestPropstatsOK(t *testing.T) {
	ms := func(statuses ...string) []byte {
		var b strings.Builder
		b.WriteString(`<d:multistatus xmlns:d="DAV:"><d:response>`)
		for _, s := range statuses {
			b.WriteString(`<d:propstat><d:status>` + s + `</d:status></d:propstat>`)
		}
		b.WriteString(`</d:response></d:multistatus>`)
		return []byte(b.String())
	}
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"set", ms("HTTP/1.1 200 OK"), true},
		{"remove", ms("HTTP/1.1 204 No Content"), true},
		{"forbidden", ms("HTTP/1.1 403 Forbidden"), false},
		// A mixed body contains "200 OK" yet must not count as accepted —
		// the false positive the old substring check would have had.
		{"mixed", ms("HTTP/1.1 200 OK", "HTTP/1.1 424 Failed Dependency"), false},
		{"malformed status", ms("garbage"), false},
		{"no propstats", ms(), false},
		{"not xml", []byte("nope"), false},
	}
	for _, c := range cases {
		if got := propstatsOK(c.body); got != c.want {
			t.Errorf("propstatsOK(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}
