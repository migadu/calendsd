package admin

// Handler tests in the established style (ADMIN-PLAN.md §6): mock backends
// plus a fake Store, httptest against the admin mux. Every HTML page gets a
// render smoke test — template field mistakes only fail at execute time.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/caldav/ical"
	"calserver/internal/carddav"
	"calserver/internal/server"
	"calserver/internal/storage"
)

const testICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:evt-1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260301T100000Z\r\nDTEND:20260301T110000Z\r\nSUMMARY:Team standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

const testVCF = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:card-1\r\nFN:Alice Example\r\nEND:VCARD\r\n"

// fakeStore serves fixed rows; freshICS-derived objects get consistent
// stored columns, the "stale.ics" object intentionally stale ones.
type fakeStore struct{}

func i64(v int64) *int64 { return &v }

func (fakeStore) Counts(ctx context.Context) (*storage.AdminCounts, error) {
	return &storage.AdminCounts{Principals: 3, Users: 2, Calendars: 1,
		CalendarInstances: 2, CalendarObjects: 2, AddressBooks: 1, Cards: 1}, nil
}

func (fakeStore) RecentChanges(ctx context.Context, limit int) ([]storage.AdminChange, error) {
	return []storage.AdminChange{
		{Kind: "calendar", CollectionID: "7", CollectionURI: "default",
			PrincipalURI: "principals/alice", ObjectURI: "a.ics", SyncToken: 4,
			Operation: 2, LastModified: i64(1750000000)},
		{Kind: "addressbook", CollectionID: "3", CollectionURI: "contacts",
			PrincipalURI: "principals/alice", ObjectURI: "gone.vcf", SyncToken: 2,
			Operation: 3},
	}, nil
}

func (fakeStore) AuthDiagnostics(ctx context.Context) (*storage.AdminAuthDiagnostics, error) {
	return &storage.AdminAuthDiagnostics{
		PrincipalsWithoutUser: []string{"principals/ghost"},
		UsersWithoutPrincipal: []string{"orphan"},
	}, nil
}

func (fakeStore) UserForPrincipal(ctx context.Context, principalURI string) (*storage.AdminUserRow, error) {
	if principalURI == "principals/alice" {
		return &storage.AdminUserRow{Username: "alice",
			DigestA1: auth.DigestA1("alice", "SabreDAV", "hunter2")}, nil
	}
	return nil, nil
}

func (fakeStore) LookupCalendar(ctx context.Context, principalURI, uri string) (string, error) {
	if principalURI == "principals/alice" && uri == "default" {
		return "7", nil
	}
	return "", nil
}

func (fakeStore) CalendarInstances(ctx context.Context, calendarID string) ([]storage.AdminCalendarInstance, error) {
	return []storage.AdminCalendarInstance{
		{InstanceID: "10", CalendarID: "7", PrincipalURI: "principals/alice",
			URI: "default", Access: 1, DisplayName: "Alice's calendar", Color: "#ff0000",
			InviteStatus: 2},
		{InstanceID: "11", CalendarID: "7", PrincipalURI: "principals/bob",
			URI: "shared-default", Access: 2, ShareHref: "mailto:bob@example.org",
			InviteStatus: 2},
	}, nil
}

func (fakeStore) CalendarMeta(ctx context.Context, calendarID string) (*storage.AdminCalendarMeta, error) {
	if calendarID != "7" {
		return nil, nil
	}
	return &storage.AdminCalendarMeta{SyncToken: 5, Components: "VEVENT,VTODO",
		ObjectCount: 2, TotalSize: int64(2 * len(testICS))}, nil
}

func (fakeStore) PropertyRows(ctx context.Context, path string) ([]storage.AdminPropRow, error) {
	return []storage.AdminPropRow{
		{Path: path, Name: "{DAV:}note", ValueType: 1, Value: "global note"},
		{Path: path + "/\nprincipals/bob", Name: "{http://apple.com/ns/ical/}calendar-color",
			ValueType: 1, Value: "#00ff00", OverlayPrincipal: "principals/bob"},
		{Path: path, Name: "{http://sabre.io}php", ValueType: 3,
			Value: "(PHP-serialized, 12 bytes)", SabreOnly: true},
	}, nil
}

func (fakeStore) AddressBook(ctx context.Context, id string) (*storage.AdminAddressBook, error) {
	if id != "3" {
		return nil, nil
	}
	return &storage.AdminAddressBook{ID: "3", PrincipalURI: "principals/alice",
		URI: "contacts", DisplayName: "Contacts", SyncToken: 2, CardCount: 1,
		TotalSize: int64(len(testVCF))}, nil
}

func (fakeStore) LookupAddressBook(ctx context.Context, principalURI, uri string) (string, error) {
	if principalURI == "principals/alice" && uri == "contacts" {
		return "3", nil
	}
	return "", nil
}

func storedRow(uri string, data string) storage.AdminObjectRow {
	den, err := ical.Denormalize([]byte(data))
	if err != nil {
		panic(err)
	}
	return storage.AdminObjectRow{URI: uri, UID: den.UID,
		ComponentType: den.ComponentType, ETag: den.ETag, Size: den.Size,
		LastModified: i64(1750000000), FirstOccurrence: den.FirstOccurrence,
		LastOccurrence: den.LastOccurrence}
}

func (fakeStore) Objects(ctx context.Context, q storage.AdminObjectQuery) ([]storage.AdminObjectRow, bool, error) {
	rows := []storage.AdminObjectRow{storedRow("a.ics", testICS)}
	if q.Match != "" && !strings.Contains("a.ics evt-1", strings.ToLower(q.Match)) {
		rows = nil
	}
	return rows, false, nil
}

func (fakeStore) Object(ctx context.Context, calendarID, uri string) (*storage.AdminObjectDetail, error) {
	switch uri {
	case "a.ics":
		return &storage.AdminObjectDetail{Row: storedRow("a.ics", testICS),
			Data: []byte(testICS)}, nil
	case "stale.ics":
		row := storedRow("stale.ics", testICS)
		row.ETag = "0000deadbeef0000"
		row.FirstOccurrence = nil
		return &storage.AdminObjectDetail{Row: row, Data: []byte(testICS)}, nil
	}
	return nil, nil
}

func (fakeStore) CardsSearch(ctx context.Context, bookID, match string, inData bool, limit int) ([]storage.AdminCardRow, bool, error) {
	return []storage.AdminCardRow{{URI: "card1.vcf", ETag: "abc",
		Size: int64(len(testVCF)), LastModified: i64(1750000000)}}, false, nil
}

func (fakeStore) Card(ctx context.Context, bookID, uri string) (*storage.AdminCardDetail, error) {
	if uri != "card1.vcf" {
		return nil, nil
	}
	card := carddav.CardInfo{CardData: []byte(testVCF)}
	return &storage.AdminCardDetail{
		Row: storage.AdminCardRow{URI: uri,
			ETag: strings.Trim(card.QuotedETag(), `"`),
			Size: int64(len(testVCF)), LastModified: i64(1750000000)},
		Data: []byte(testVCF)}, nil
}

func testApp() *App {
	principals := &acl.MemoryBackend{
		Principals: []acl.Principal{
			{URI: "principals/alice", Props: map[xmlName]any{
				{Space: "DAV:", Local: "displayname"}:                     "Alice",
				{Space: "http://sabredav.org/ns", Local: "email-address"}: "alice@example.org",
			}},
			{URI: "principals/bob"},
			{URI: "principals/ghost"},
		},
		Groups: map[string][]string{
			// Alice may read Bob's calendars; Bob may write Alice's.
			"principals/bob/calendar-proxy-read":    {"principals/alice"},
			"principals/alice/calendar-proxy-write": {"principals/bob"},
		},
	}
	calendars := &caldav.MockBackend{Calendars: []caldav.CalendarInfo{{
		ID: caldav.CalendarID{Calendar: "7", Instance: "10"}, URI: "default",
		PrincipalURI: "principals/alice",
		Props:        map[xmlName]any{{Space: "DAV:", Local: "displayname"}: "Alice's calendar"},
	}}}
	books := &carddav.MockBackend{AddressBooks: []carddav.AddressBookInfo{{
		ID: "3", URI: "contacts", PrincipalURI: "principals/alice",
		Props: map[xmlName]any{{Space: "DAV:", Local: "displayname"}: "Contacts"},
	}}}
	return &App{
		Store:        fakeStore{},
		Principals:   principals,
		Calendars:    calendars,
		AddressBooks: books,
		Config:       server.Config{BaseURI: "/", Realm: "SabreDAV"},
		ReadOnly:     true,
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func getJSON[T any](t *testing.T, h http.Handler, path string) T {
	t.Helper()
	w := get(t, h, path)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("GET %s: bad JSON: %v", path, err)
	}
	return v
}

func TestDashboardJSON(t *testing.T) {
	h := testApp().Handler()
	d := getJSON[DashboardData](t, h, "/api/dashboard")
	if d.Counts.Principals != 3 || !d.ReadOnly {
		t.Fatalf("dashboard = %+v", d)
	}
	if !strings.Contains(d.ConfigLine, "realm=SabreDAV") {
		t.Fatalf("config line %q missing realm", d.ConfigLine)
	}
	if len(d.Changes) != 2 || d.Changes[1].LastModified != nil {
		t.Fatalf("changes = %+v (delete must have nil lastModified)", d.Changes)
	}
	if d.Diagnostics.PrincipalsWithoutUser[0] != "principals/ghost" {
		t.Fatalf("diagnostics = %+v", d.Diagnostics)
	}
}

func TestPrincipalDelegationRendering(t *testing.T) {
	h := testApp().Handler()
	d := getJSON[PrincipalDetail](t, h, "/api/principal?path=principals/alice")
	if d.User == nil || d.User.Username != "alice" || !d.DigestWellFormed {
		t.Fatalf("user = %+v wellformed=%t", d.User, d.DigestWellFormed)
	}
	if len(d.SeesCalendarsOfRead) != 1 || d.SeesCalendarsOfRead[0] != "principals/bob" {
		t.Fatalf("seesCalendarsOfRead = %v, want [principals/bob]", d.SeesCalendarsOfRead)
	}
	if len(d.WriteDelegates) != 1 || d.WriteDelegates[0] != "principals/bob" {
		t.Fatalf("writeDelegates = %v, want [principals/bob]", d.WriteDelegates)
	}
	if len(d.Groups) != 0 {
		t.Fatalf("proxy groups leaked into plain groups: %v", d.Groups)
	}
	if len(d.Calendars) != 1 || d.Calendars[0].ID != "7" {
		t.Fatalf("calendars = %+v", d.Calendars)
	}

	// Ghost has no users row.
	g := getJSON[PrincipalDetail](t, h, "/api/principal?path=principals/ghost")
	if g.User != nil {
		t.Fatalf("ghost user = %+v, want nil", g.User)
	}

	if w := get(t, h, "/api/principal?path=principals/nobody"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown principal = %d, want 404", w.Code)
	}
	if w := get(t, h, "/api/principal"); w.Code != http.StatusBadRequest {
		t.Fatalf("missing path = %d, want 400", w.Code)
	}
}

func TestCalendarInspector(t *testing.T) {
	h := testApp().Handler()
	d := getJSON[CalendarDetail](t, h, "/api/calendar?principal=principals/alice&uri=default")
	if d.ID != "7" || d.Meta.SyncToken != 5 || len(d.Instances) != 2 {
		t.Fatalf("calendar = %+v", d)
	}
	if d.Instances[0].Path != "calendars/alice/default" {
		t.Fatalf("owner instance path = %q", d.Instances[0].Path)
	}
	if d.Instances[1].Path != "calendars/bob/shared-default" {
		t.Fatalf("shared instance path = %q", d.Instances[1].Path)
	}
	var overlay, sabreOnly bool
	for _, p := range d.Instances[0].PropRows {
		overlay = overlay || p.OverlayPrincipal == "principals/bob"
		sabreOnly = sabreOnly || p.SabreOnly
	}
	if !overlay || !sabreOnly {
		t.Fatalf("prop layers missing: overlay=%t sabreOnly=%t", overlay, sabreOnly)
	}
	if w := get(t, h, "/api/calendar?id=999"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown calendar = %d, want 404", w.Code)
	}
}

func TestObjectStoredVsDerived(t *testing.T) {
	h := testApp().Handler()
	fresh := getJSON[ObjectDetail](t, h, "/api/object?calendar=7&uri=a.ics")
	if fresh.ParseError != "" || len(fresh.Diffs) != 0 {
		t.Fatalf("fresh object diffs = %v parseError=%q, want none", fresh.Diffs, fresh.ParseError)
	}
	stale := getJSON[ObjectDetail](t, h, "/api/object?calendar=7&uri=stale.ics")
	want := map[string]bool{"etag": true, "firstoccurence": true}
	if len(stale.Diffs) != len(want) {
		t.Fatalf("stale diffs = %v, want etag+firstoccurence", stale.Diffs)
	}
	for _, d := range stale.Diffs {
		if !want[d] {
			t.Fatalf("stale diffs = %v, want etag+firstoccurence", stale.Diffs)
		}
	}
}

func TestCheckCredentials(t *testing.T) {
	h := testApp().Handler()
	post := func(form url.Values, withHeader bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/check-credentials",
			strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withHeader {
			req.Header.Set("X-Admin-Request", "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	form := url.Values{"username": {"alice"}, "password": {"hunter2"}}

	if w := post(form, false); w.Code != http.StatusForbidden {
		t.Fatalf("without CSRF header = %d, want 403", w.Code)
	}

	var res CheckCredentialsResult
	w := post(form, true)
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Found || !res.Match {
		t.Fatalf("valid credentials: %+v", res)
	}

	w = post(url.Values{"username": {"alice"}, "password": {"wrong"}}, true)
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Found || res.Match {
		t.Fatalf("wrong password: %+v", res)
	}
	if !strings.Contains(res.Note, "different realm") {
		t.Fatalf("note %q should mention the realm-mismatch possibility", res.Note)
	}

	w = post(url.Values{"username": {"nobody"}, "password": {"x"}}, true)
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Found || res.Match {
		t.Fatalf("unknown user: %+v", res)
	}
}

// TestHTMLPagesRender executes every page template against realistic data;
// a template referencing a renamed field fails here, not in production.
func TestHTMLPagesRender(t *testing.T) {
	h := testApp().Handler()
	pages := map[string]string{
		"/":                                "Dashboard",
		"/principals":                      "principals/alice",
		"/principal?path=principals/alice": "Credential check",
		"/calendar?id=7":                   "calendars/alice/default",
		"/addressbook?id=3":                "Contacts",
		"/objects?calendar=7":              "a.ics",
		"/object?calendar=7&uri=a.ics":     "Team standup",
		"/object?calendar=7&uri=stale.ics": "stale",
		"/cards?book=3":                    "card1.vcf",
		"/card?book=3&uri=card1.vcf":       "Alice Example",
	}
	for path, marker := range pages {
		w := get(t, h, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, w.Code, w.Body.String())
			continue
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s content-type = %q", path, ct)
		}
		if !strings.Contains(w.Body.String(), marker) {
			t.Errorf("GET %s: page does not contain %q", path, marker)
		}
	}
}
