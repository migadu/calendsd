package admin

// Phase C handler tests: gating (CSRF header, -readonly, nil Writes),
// digest minting, delegation validation, proppatch/provision through the
// mock production chain, orphan deletion pass-through.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/carddav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	"calserver/internal/storage"
)

// fakeWrites records the carve-out calls.
type fakeWrites struct {
	created   []string // "username digestA1 email displayName"
	passwords map[string]string
	deleted   []string
	removed   []string // usernames passed to DeleteUser (guarded)
	cascaded  []string // usernames passed to DeleteUserCascade
	scheduled []string // "username days" passed to ScheduleDelete
	restored  []string // usernames passed to CancelDelete
	deletions []storage.DeletedPrincipal
}

func (f *fakeWrites) CreateUser(username, digestA1, email, displayName string) error {
	f.created = append(f.created, strings.Join([]string{username, digestA1, email, displayName}, " "))
	return nil
}

func (f *fakeWrites) SetPassword(username, digestA1 string) error {
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	f.passwords[username] = digestA1
	return nil
}

func (f *fakeWrites) DeleteUser(username string) error {
	f.removed = append(f.removed, username)
	return nil
}

func (f *fakeWrites) DeleteUserCascade(username string) error {
	f.cascaded = append(f.cascaded, username)
	return nil
}

func (f *fakeWrites) ScheduleDelete(username string, days int, reason string) error {
	f.scheduled = append(f.scheduled, fmt.Sprintf("%s %d %s", username, days, reason))
	return nil
}

func (f *fakeWrites) CancelDelete(username string) error {
	f.restored = append(f.restored, username)
	return nil
}

func (f *fakeWrites) Deletions() ([]storage.DeletedPrincipal, error) {
	return f.deletions, nil
}

func (f *fakeWrites) DeleteOrphans(check string) (int64, error) {
	f.deleted = append(f.deleted, check)
	return 2, nil
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values, withHeader bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if withHeader {
		req.Header.Set("X-Admin-Request", "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestWriteGating(t *testing.T) {
	a := testAppB()
	a.Writes = &fakeWrites{}
	form := url.Values{"username": {"x"}, "password": {"y"}}

	// Read-only (the default) refuses even with the CSRF header.
	a.ReadOnly = true
	if w := postForm(t, a.Handler(), "/api/create-user", form, true); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "read-only") {
		t.Fatalf("readonly create-user = %d: %s", w.Code, w.Body.String())
	}

	// Missing CSRF header refuses regardless.
	a.ReadOnly = false
	if w := postForm(t, a.Handler(), "/api/create-user", form, false); w.Code != http.StatusForbidden {
		t.Fatalf("create-user without CSRF header = %d", w.Code)
	}

	// Nil Writes refuses carve-out actions.
	a.Writes = nil
	if w := postForm(t, a.Handler(), "/api/create-user", form, true); w.Code != http.StatusForbidden {
		t.Fatalf("create-user with nil Writes = %d", w.Code)
	}
}

func TestCreateUserAndSetPassword(t *testing.T) {
	a := testAppB()
	writes := &fakeWrites{}
	a.Writes = writes
	a.ReadOnly = false
	h := a.Handler()

	w := postForm(t, h, "/api/create-user",
		url.Values{"username": {"carol"}, "password": {"pw1"},
			"email": {"carol@example.org"}, "displayname": {"Carol"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("create-user = %d: %s", w.Code, w.Body.String())
	}
	wantDigest := auth.DigestA1("carol", "SabreDAV", "pw1")
	if len(writes.created) != 1 || writes.created[0] != "carol "+wantDigest+" carol@example.org Carol" {
		t.Fatalf("created = %v (digest must be minted under the configured realm)", writes.created)
	}

	w = postForm(t, h, "/api/set-password",
		url.Values{"username": {"carol"}, "password": {"pw2"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("set-password = %d: %s", w.Code, w.Body.String())
	}
	if writes.passwords["carol"] != auth.DigestA1("carol", "SabreDAV", "pw2") {
		t.Fatalf("stored digest = %q", writes.passwords["carol"])
	}

	if w := postForm(t, h, "/api/create-user", url.Values{"username": {"x"}}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("create-user without password = %d", w.Code)
	}
}

func TestSetDelegation(t *testing.T) {
	a := testAppB()
	a.Writes = &fakeWrites{}
	a.ReadOnly = false
	h := a.Handler()

	w := postForm(t, h, "/api/set-delegation",
		url.Values{"principal": {"principals/alice"}, "kind": {"read"},
			"members": {"principals/bob  principals/ghost"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("set-delegation = %d: %s", w.Code, w.Body.String())
	}
	members, _ := a.Principals.GroupMemberSet("principals/alice/calendar-proxy-read")
	if len(members) != 2 || members[0] != "principals/bob" {
		t.Fatalf("member set = %v", members)
	}

	// Unknown member principals are rejected before any write.
	w = postForm(t, h, "/api/set-delegation",
		url.Values{"principal": {"principals/alice"}, "kind": {"write"},
			"members": {"principals/typo"}}, true)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "principals/typo") {
		t.Fatalf("unknown member = %d: %s", w.Code, w.Body.String())
	}
	// The fixture's pre-existing write set must survive untouched.
	if members, _ := a.Principals.GroupMemberSet("principals/alice/calendar-proxy-write"); len(members) != 1 || members[0] != "principals/bob" {
		t.Fatalf("write proxy set mutated on rejected request: %v", members)
	}

	if w := postForm(t, h, "/api/set-delegation",
		url.Values{"principal": {"principals/alice"}, "kind": {"admin"}}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("bad kind = %d", w.Code)
	}
}

func TestPropPatchThroughChain(t *testing.T) {
	a := testAppB()
	a.Writes = &fakeWrites{}
	a.ReadOnly = false
	factory, backend := newMockSimulatorEnv()
	a.NewSimulator = factory
	h := a.Handler()

	w := postForm(t, h, "/api/proppatch",
		url.Values{"path": {"/calendars/alice/default"},
			"name": {"{DAV:}displayname"}, "value": {"Renamed"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("proppatch = %d: %s", w.Code, w.Body.String())
	}

	// The mock backend must have received the update through UpdateCalendar.
	cals, _ := backend.CalendarsForUser("principals/alice")
	if got := cals[0].Props[xmlName{Space: "DAV:", Local: "displayname"}]; got != "Renamed" {
		t.Fatalf("displayname after proppatch = %v", got)
	}

	if w := postForm(t, h, "/api/proppatch",
		url.Values{"path": {"/x"}, "name": {"displayname"}}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("non-clark name = %d", w.Code)
	}
}

func TestProvision(t *testing.T) {
	a := testAppB()
	a.Writes = &fakeWrites{}
	a.ReadOnly = false
	// A provisioner whose backends start empty for bob, with auto-provision
	// on — the production CalendarRoot creates the default calendar.
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/admin"}, {URI: "principals/bob"},
	}}
	calBackend := &caldav.MockBackend{}
	abBackend := &carddav.MockBackend{}
	a.NewProvisioner = func(principal string) *dav.Server {
		root := mock.NewCollection("root")
		root.Attach(acl.NewPrincipalCollection(principals, "principals"))
		calRoot := caldav.NewCalendarRoot(principals, calBackend, "calendars")
		calRoot.AutoProvision = true
		root.Attach(calRoot)
		abRoot := carddav.NewAddressBookRoot(principals, abBackend, "addressbooks")
		abRoot.AutoProvision = true
		root.Attach(abRoot)
		srv := dav.NewServer("/", root)
		srv.AddPlugin(auth.NewLoggedInPlugin(principal))
		srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
		srv.AddPlugin(&caldav.Plugin{})
		srv.AddPlugin(&carddav.Plugin{})
		return srv
	}
	h := a.Handler()

	w := postForm(t, h, "/api/provision",
		url.Values{"principal": {"principals/bob"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("provision = %d: %s", w.Code, w.Body.String())
	}
	cals, _ := calBackend.CalendarsForUser("principals/bob")
	if len(cals) != 1 {
		t.Fatalf("no calendar auto-provisioned for bob: %v", cals)
	}
	books, _ := abBackend.AddressBooksForUser("principals/bob")
	if len(books) != 1 {
		t.Fatalf("no address book auto-provisioned for bob: %v", books)
	}

	if w := postForm(t, h, "/api/provision",
		url.Values{"principal": {"bob"}}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("bare username accepted = %d", w.Code)
	}
}

func TestDeleteOrphansEndpoint(t *testing.T) {
	a := testAppB()
	writes := &fakeWrites{}
	a.Writes = writes
	a.ReadOnly = false
	h := a.Handler()

	w := postForm(t, h, "/api/delete-orphans", url.Values{"check": {"orphaned-instance"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("delete-orphans = %d: %s", w.Code, w.Body.String())
	}
	var res writeResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || !strings.Contains(res.Detail, "2 row(s)") {
		t.Fatalf("result = %+v", res)
	}
	if len(writes.deleted) != 1 || writes.deleted[0] != "orphaned-instance" {
		t.Fatalf("deleted = %v", writes.deleted)
	}
}

func TestDeleteUserEndpoint(t *testing.T) {
	a := testAppB()
	writes := &fakeWrites{}
	a.Writes = writes
	a.ReadOnly = false
	h := a.Handler()

	// Default (no cascade) routes to the guarded DeleteUser.
	w := postForm(t, h, "/api/delete-user", url.Values{"username": {"carol"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("delete-user = %d: %s", w.Code, w.Body.String())
	}
	if len(writes.removed) != 1 || writes.removed[0] != "carol" || len(writes.cascaded) != 0 {
		t.Fatalf("removed=%v cascaded=%v, want guarded delete of carol", writes.removed, writes.cascaded)
	}

	// cascade=1 routes to DeleteUserCascade instead.
	w = postForm(t, h, "/api/delete-user", url.Values{"username": {"dave"}, "cascade": {"1"}}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("delete-user cascade = %d: %s", w.Code, w.Body.String())
	}
	if len(writes.cascaded) != 1 || writes.cascaded[0] != "dave" || len(writes.removed) != 1 {
		t.Fatalf("removed=%v cascaded=%v, want cascade delete of dave", writes.removed, writes.cascaded)
	}

	// Username is required.
	if w := postForm(t, h, "/api/delete-user", url.Values{}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("delete-user without username = %d", w.Code)
	}

	// Gated like every other write: read-only refuses.
	a.ReadOnly = true
	if w := postForm(t, a.Handler(), "/api/delete-user", url.Values{"username": {"carol"}}, true); w.Code != http.StatusForbidden {
		t.Fatalf("delete-user in read-only = %d", w.Code)
	}
}

func TestSoftDeleteEndpoints(t *testing.T) {
	a := testAppB()
	writes := &fakeWrites{}
	a.Writes = writes
	a.ReadOnly = false
	h := a.Handler()

	// No days → default from config (0 → built-in 30).
	if w := postForm(t, h, "/api/schedule-delete", url.Values{"username": {"carol"}}, true); w.Code != http.StatusOK {
		t.Fatalf("schedule-delete = %d: %s", w.Code, w.Body.String())
	}
	if len(writes.scheduled) != 1 || writes.scheduled[0] != "carol 30 " {
		t.Fatalf("scheduled = %v, want default 30-day window", writes.scheduled)
	}

	// Explicit days + reason pass through.
	if w := postForm(t, h, "/api/schedule-delete",
		url.Values{"username": {"dave"}, "days": {"7"}, "reason": {"abuse"}}, true); w.Code != http.StatusOK {
		t.Fatalf("schedule-delete days = %d: %s", w.Code, w.Body.String())
	}
	if writes.scheduled[1] != "dave 7 abuse" {
		t.Fatalf("scheduled = %v", writes.scheduled)
	}

	// Invalid days rejected before any write.
	if w := postForm(t, h, "/api/schedule-delete",
		url.Values{"username": {"dave"}, "days": {"0"}}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("schedule-delete days=0 = %d", w.Code)
	}
	if w := postForm(t, h, "/api/schedule-delete", url.Values{}, true); w.Code != http.StatusBadRequest {
		t.Fatalf("schedule-delete without username = %d", w.Code)
	}

	// Restore routes to CancelDelete.
	if w := postForm(t, h, "/api/restore-user", url.Values{"username": {"carol"}}, true); w.Code != http.StatusOK {
		t.Fatalf("restore-user = %d: %s", w.Code, w.Body.String())
	}
	if len(writes.restored) != 1 || writes.restored[0] != "carol" {
		t.Fatalf("restored = %v", writes.restored)
	}

	// Both gated: read-only refuses.
	a.ReadOnly = true
	if w := postForm(t, a.Handler(), "/api/schedule-delete", url.Values{"username": {"carol"}}, true); w.Code != http.StatusForbidden {
		t.Fatalf("schedule-delete in read-only = %d", w.Code)
	}
	if w := postForm(t, a.Handler(), "/api/restore-user", url.Values{"username": {"carol"}}, true); w.Code != http.StatusForbidden {
		t.Fatalf("restore-user in read-only = %d", w.Code)
	}
}

func TestPhaseCHTMLForms(t *testing.T) {
	a := testAppB()
	a.Writes = &fakeWrites{}
	h := a.Handler()
	pages := map[string]string{
		"/principals":                      "Create user",
		"/principal?path=principals/alice": "Setup actions",
		"/calendar?id=7":                   "Fix a property",
		"/addressbook?id=3":                "Fix a property",
		"/doctor":                          "delete orphaned-instance",
	}
	for path, marker := range pages {
		w := get(t, h, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, w.Code, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), marker) {
			t.Errorf("GET %s: page does not contain %q", path, marker)
		}
	}

	// The principal page carries the danger-zone forms: immediate delete
	// (cascade toggle) and, for a live account, the schedule-deletion form.
	body := get(t, h, "/principal?path=principals/alice").Body.String()
	for _, marker := range []string{"/api/delete-user", "cascade", "Delete user",
		"/api/schedule-delete", "Schedule deletion"} {
		if !strings.Contains(body, marker) {
			t.Errorf("principal page missing danger-zone marker %q", marker)
		}
	}

	// When the account is already scheduled, the page shows the purge date and
	// a restore form instead of the schedule form.
	scheduled := testAppB()
	scheduled.Writes = &fakeWrites{deletions: []storage.DeletedPrincipal{
		{Principal: "principals/alice", PurgeAfter: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)},
	}}
	sbody := get(t, scheduled.Handler(), "/principal?path=principals/alice").Body.String()
	for _, marker := range []string{"/api/restore-user", "Restore user", "2026-08-30"} {
		if !strings.Contains(sbody, marker) {
			t.Errorf("scheduled principal page missing marker %q", marker)
		}
	}
	if strings.Contains(sbody, "/api/schedule-delete") {
		t.Error("scheduled principal page still shows the schedule form")
	}
}
