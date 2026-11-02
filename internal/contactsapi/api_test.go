package contactsapi_test

// Contacts API tests against a scratch Postgres carrying the sabre schema and
// calendsd's extension tables (contact_frecency lives there). Skips when the
// dev Postgres is not reachable, like the storage tests.
//
// The auth backend is a fake: the API's contract with it is the auth.Backend
// interface, and the real rcptd backend has its own tests. What is asserted
// here is the surface SnappyMail's plugin will program against — status codes,
// the id scheme, If-Match, the create target, the scope of delete-all — and the
// invariants the doc leans on: a create never lands outside the webmail book,
// a stale If-Match never clobbers, a principal never sees another's cards.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/contactsapi"
	"github.com/migadu/calendsd/internal/storage"
	"github.com/migadu/calendsd/internal/storage/storagetest"
)

func openScratchDB(t *testing.T, dbName string) *sql.DB {
	return storagetest.NewDB(t, dbName)
}

// fakeAuth accepts any user with the one password and maps them to
// principals/<user>, lowercased — the shape RcptdBackend produces.
type fakeAuth struct{}

func (fakeAuth) Check(r *http.Request) (string, error) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return "", errors.New("no credentials")
	}
	if pass != "pw" {
		return "", auth.ErrBadCredentials
	}
	return "principals/" + strings.ToLower(user), nil
}

func (fakeAuth) Challenge(h http.Header, _ *http.Request) {
	h.Add("WWW-Authenticate", `Basic realm="test"`)
}

type env struct {
	t    *testing.T
	db   *sql.DB
	h    http.Handler
	opts contactsapi.Options
}

func newEnv(t *testing.T, dbName string, opts contactsapi.Options) *env {
	t.Helper()
	db := openScratchDB(t, dbName)
	users := &storage.Users{DB: db}
	tomb := func(ctx context.Context, p string) bool { ok, _ := users.IsDeleted(ctx, p); return ok }
	return &env{t: t, db: db, h: contactsapi.New(db, fakeAuth{}, tomb, opts), opts: opts}
}

// principal creates the user the way first login would: principal rows only,
// no address book.
func (e *env) principal(address string) {
	e.t.Helper()
	w := &storage.AdminWrites{DB: e.db}
	if err := w.CreateUser(context.Background(), address, storage.BlockedDigestA1(), address, address); err != nil {
		e.t.Fatal(err)
	}
}

type resp struct {
	code int
	body []byte
	hdr  http.Header
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decoding %q: %v", r.body, err)
	}
}

func (e *env) do(user, method, path string, body any, headers ...string) resp {
	e.t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if user != "" {
		req.SetBasicAuth(user, "pw")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return resp{code: w.Code, body: w.Body.Bytes(), hdr: w.Header()}
}

func (e *env) want(r resp, code int) resp {
	e.t.Helper()
	if r.code != code {
		e.t.Fatalf("status = %d, want %d; body: %s", r.code, code, r.body)
	}
	return r
}

// idOf is the cards row id for a uri, for cards created behind the API's back.
func (e *env) idOf(uri string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.QueryRow(`SELECT id FROM cards WHERE uri = $1`, uri).Scan(&id); err != nil {
		e.t.Fatalf("id of %s: %v", uri, err)
	}
	return id
}

func path(id int64) string { return fmt.Sprintf("/api/v1/contacts/%d", id) }

func vcf(uid, fn string, emails ...string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + uid + "\r\nFN:" + fn + "\r\n")
	for _, e := range emails {
		b.WriteString("EMAIL:" + e + "\r\n")
	}
	b.WriteString("END:VCARD\r\n")
	return b.String()
}

type contactJSON struct {
	ID      int64  `json:"id"`
	Book    string `json:"book"`
	URI     string `json:"uri"`
	UID     string `json:"uid"`
	ETag    string `json:"etag"`
	Changed int64  `json:"changed"`
	VCard   string `json:"vcard"`
}

type listJSON struct {
	Total    int           `json:"total"`
	Contacts []contactJSON `json:"contacts"`
}

func TestAuthGate(t *testing.T) {
	e := newEnv(t, "calserver_capi_auth", contactsapi.Options{})
	e.principal("alice@example.org")

	r := e.do("", "GET", "/api/v1/health", nil)
	e.want(r, http.StatusUnauthorized)
	if r.hdr.Get("WWW-Authenticate") == "" {
		t.Error("401 without a WWW-Authenticate challenge")
	}
	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	req.SetBasicAuth("alice@example.org", "wrong")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", w.Code)
	}

	var health struct {
		OK        bool   `json:"ok"`
		Principal string `json:"principal"`
	}
	e.want(e.do("Alice@Example.org", "GET", "/api/v1/health", nil), 200).json(t, &health)
	if !health.OK || health.Principal != "principals/alice@example.org" {
		t.Errorf("health = %+v", health)
	}

	// Valid credentials, account scheduled for deletion: 403, and nothing runs
	// as them — the same gate DAV applies.
	e.principal("erin@example.org")
	if err := (&storage.AdminWrites{DB: e.db}).ScheduleDelete(req.Context(), "erin@example.org", 30, "test"); err != nil {
		t.Fatal(err)
	}
	e.want(e.do("erin@example.org", "GET", "/api/v1/contacts", nil), http.StatusForbidden)
}

func TestContactLifecycle(t *testing.T) {
	e := newEnv(t, "calserver_capi_lifecycle", contactsapi.Options{})
	e.principal("alice@example.org")
	const alice = "alice@example.org"

	// A user with no address book at all: the create makes "webmail".
	var created contactJSON
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("u1", "Bob Builder", "Bob@Example.net")}), 201).json(t, &created)
	if created.ID <= 0 || created.Book != "webmail" || created.URI != "u1.vcf" || created.UID != "u1" || len(created.ETag) != 32 || created.Changed == 0 {
		t.Fatalf("created = %+v", created)
	}
	// Creating it again is a conflict, not a silent overwrite — and the 409
	// names the card in the way, so a client can update exactly that one.
	var conflict struct {
		ID   int64  `json:"id"`
		ETag string `json:"etag"`
	}
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("u1", "Bob Again")}), http.StatusConflict).json(t, &conflict)
	if conflict.ID != created.ID || conflict.ETag != created.ETag {
		t.Errorf("409 = %+v, want id %d etag %s", conflict, created.ID, created.ETag)
	}

	var list listJSON
	e.want(e.do(alice, "GET", "/api/v1/contacts", nil), 200).json(t, &list)
	if list.Total != 1 || len(list.Contacts) != 1 || list.Contacts[0].ID != created.ID {
		t.Fatalf("list = %+v", list)
	}

	var got contactJSON
	e.want(e.do(alice, "GET", path(created.ID), nil), 200).json(t, &got)
	if got.ETag != created.ETag || !strings.Contains(got.VCard, "FN:Bob Builder") {
		t.Errorf("get = %+v", got)
	}
	e.want(e.do(alice, "GET", "/api/v1/contacts/by-uid/u1", nil), 200)
	e.want(e.do(alice, "GET", "/api/v1/contacts/by-uid/nope", nil), 404)
	// Address lookup is case-insensitive and goes in a query parameter.
	e.want(e.do(alice, "GET", "/api/v1/contacts/by-email?email=bob@example.net", nil), 200)
	e.want(e.do(alice, "GET", "/api/v1/contacts/by-email?email=nobody@example.net", nil), 404)

	// Another principal sees none of it: the principal is part of every query.
	e.principal("mallory@example.org")
	e.want(e.do("mallory@example.org", "GET", path(created.ID), nil), 404)
	e.want(e.do(alice, "GET", "/api/v1/contacts/not-a-number", nil), http.StatusBadRequest)
	e.want(e.do("mallory@example.org", "GET", "/api/v1/contacts", nil), 200).json(t, &list)
	if list.Total != 0 {
		t.Errorf("mallory sees %d of alice's contacts", list.Total)
	}

	// Update with the right If-Match: new etag, new bytes.
	var updated contactJSON
	e.want(e.do(alice, "PUT", path(created.ID),
		map[string]string{"vcard": vcf("u1", "Bob the Builder", "bob@example.net")},
		"If-Match", `"`+created.ETag+`"`), 200).json(t, &updated)
	if updated.ETag == created.ETag || !strings.Contains(updated.VCard, "FN:Bob the Builder") {
		t.Errorf("updated = %+v", updated)
	}
	// The stale etag (the phone-edited-it-first case) is refused, and the
	// stored card is untouched.
	e.want(e.do(alice, "PUT", path(created.ID),
		map[string]string{"vcard": vcf("u1", "Clobber")},
		"If-Match", `"`+created.ETag+`"`), http.StatusPreconditionFailed)
	e.want(e.do(alice, "GET", path(created.ID), nil), 200).json(t, &got)
	if got.ETag != updated.ETag {
		t.Errorf("a refused update changed the card: etag %s, want %s", got.ETag, updated.ETag)
	}
	// If-Match: * and no If-Match are unconditional.
	e.want(e.do(alice, "PUT", path(created.ID),
		map[string]string{"vcard": vcf("u1", "Bob Star")}, "If-Match", "*"), 200)
	e.want(e.do(alice, "PUT", path(created.ID),
		map[string]string{"vcard": vcf("u1", "Bob Plain")}), 200)
	// A card whose UID changes under its uri is refused.
	e.want(e.do(alice, "PUT", path(created.ID),
		map[string]string{"vcard": vcf("other", "Bob")}), http.StatusBadRequest)
	e.want(e.do(alice, "PUT", path(created.ID+1000),
		map[string]string{"vcard": vcf("missing", "Nobody")}), 404)

	// Every write left a change-log row and moved the synctoken, so a DAV
	// client that synced before learns of each one.
	var changes int
	if err := e.db.QueryRow(`SELECT count(*) FROM addressbookchanges`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if changes != 4 { // create + 3 successful updates
		t.Errorf("change rows = %d, want 4", changes)
	}

	e.want(e.do(alice, "DELETE", path(created.ID), nil), http.StatusNoContent)
	e.want(e.do(alice, "DELETE", path(created.ID), nil), 404)
	e.want(e.do(alice, "GET", "/api/v1/contacts", nil), 200).json(t, &list)
	if list.Total != 0 {
		t.Errorf("after delete total = %d", list.Total)
	}
}

func TestCreateRejectsUnusableCards(t *testing.T) {
	e := newEnv(t, "calserver_capi_badcard", contactsapi.Options{MaxBodyBytes: 4096})
	e.principal("alice@example.org")
	const alice = "alice@example.org"

	for name, card := range map[string]string{
		"no uid":       "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:X\r\nEND:VCARD\r\n",
		"slash in uid": vcf("a/b", "X"),
		"space in uid": vcf("a b", "X"),
		"not a vcard":  "hello",
		"dot-led uid":  vcf(".hidden", "X"),
	} {
		if r := e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": card}); r.code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, r.code, r.body)
		}
	}
	e.want(e.do(alice, "POST", "/api/v1/contacts", `{"vcard": "x", "extra": 1}`), http.StatusBadRequest)
	e.want(e.do(alice, "POST", "/api/v1/contacts", "{not json"), http.StatusBadRequest)
	// Over the body cap: 413, not a silent truncation.
	big := map[string]string{"vcard": vcf("u9", strings.Repeat("x", 5000))}
	e.want(e.do(alice, "POST", "/api/v1/contacts", big), http.StatusRequestEntityTooLarge)
}

func TestCreateTargetIsSoleBookWhenNoWebmail(t *testing.T) {
	e := newEnv(t, "calserver_capi_target", contactsapi.Options{})
	e.principal("carol@example.org")
	const carol = "carol@example.org"
	cardDAV := &storage.CardDAV{DB: e.db}
	// A native calendsd user: one book, made by DAV auto-provisioning.
	if _, err := cardDAV.CreateAddressBook(t.Context(), "principals/carol@example.org", "addressbook", nil); err != nil {
		t.Fatal(err)
	}
	var created contactJSON
	e.want(e.do(carol, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("c1", "Dave")}), 201).json(t, &created)
	if created.Book != "addressbook" || created.URI != "c1.vcf" {
		t.Errorf("created in %q, want the sole book: %+v", created.Book, created)
	}
	// With two books and neither named webmail, a webmail book is created
	// rather than guessing between them.
	if _, err := cardDAV.CreateAddressBook(t.Context(), "principals/carol@example.org", "second", nil); err != nil {
		t.Fatal(err)
	}
	e.want(e.do(carol, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("c2", "Eve")}), 201).json(t, &created)
	if created.Book != "webmail" {
		t.Errorf("created in %q, want webmail", created.Book)
	}
}

// TestConflictNamesTheWebmailCopy is the bug the 409 body exists for: the same
// UID in two books, and a create that conflicts. Before the 409 carried an id,
// the plugin looked the card up by UID, which searched every book in name
// order and found the copy in "addressbook" — so the update went to the wrong
// card and the webmail one stayed as it was.
func TestConflictNamesTheWebmailCopy(t *testing.T) {
	e := newEnv(t, "calserver_capi_conflict", contactsapi.Options{})
	e.principal("alice@example.org")
	const alice = "alice@example.org"
	cardDAV := &storage.CardDAV{DB: e.db}
	// "addressbook" sorts before "webmail"; give it a card with the same UID.
	otherID, err := cardDAV.CreateAddressBook(t.Context(), "principals/alice@example.org", "addressbook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cardDAV.CreateCard(t.Context(), otherID, "dup.vcf", []byte(vcf("dup", "Phone Copy"))); err != nil {
		t.Fatal(err)
	}
	if _, err := cardDAV.CreateAddressBook(t.Context(), "principals/alice@example.org", "webmail", nil); err != nil {
		t.Fatal(err)
	}
	var created contactJSON
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("dup", "Webmail Copy")}), 201).json(t, &created)
	if created.Book != "webmail" {
		t.Fatalf("created in %q, want webmail", created.Book)
	}
	phoneID := e.idOf("dup.vcf")
	if phoneID == created.ID {
		// idOf takes the first row; make sure we hold the addressbook copy.
		if err := e.db.QueryRow(`SELECT c.id FROM cards c JOIN addressbooks a ON a.id = c.addressbookid WHERE c.uri = 'dup.vcf' AND a.uri = 'addressbook'`).Scan(&phoneID); err != nil {
			t.Fatal(err)
		}
	}

	var conflict struct {
		ID   int64  `json:"id"`
		ETag string `json:"etag"`
	}
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("dup", "Webmail Again")}), http.StatusConflict).json(t, &conflict)
	if conflict.ID != created.ID || conflict.ID == phoneID || conflict.ETag != created.ETag {
		t.Errorf("409 names id %d (etag %s); want the webmail copy %d, not the addressbook copy %d", conflict.ID, conflict.ETag, created.ID, phoneID)
	}
	// And the read-side lookup by UID prefers the webmail copy too, rather
	// than whichever book name sorts first.
	var byUID contactJSON
	e.want(e.do(alice, "GET", "/api/v1/contacts/by-uid/dup", nil), 200).json(t, &byUID)
	if byUID.ID != created.ID {
		t.Errorf("by-uid = id %d in %q, want the webmail copy %d", byUID.ID, byUID.Book, created.ID)
	}
}

func TestUpdateEdgeCases(t *testing.T) {
	e := newEnv(t, "calserver_capi_update", contactsapi.Options{})
	e.principal("alice@example.org")
	const alice = "alice@example.org"
	cardDAV := &storage.CardDAV{DB: e.db}
	bookID, err := cardDAV.CreateAddressBook(t.Context(), "principals/alice@example.org", "webmail", nil)
	if err != nil {
		t.Fatal(err)
	}

	// A card a phone named without regard to its UID is listed under its real
	// file name and must stay editable; only a CHANGE of UID is refused.
	phone := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:urn:uuid:0e1\r\nFN:Phone Named\r\nEND:VCARD\r\n"
	if _, err := cardDAV.CreateCard(t.Context(), bookID, "ABC123.vcf", []byte(phone)); err != nil {
		t.Fatal(err)
	}
	phoneID := e.idOf("ABC123.vcf")
	var got contactJSON
	e.want(e.do(alice, "GET", path(phoneID), nil), 200).json(t, &got)
	if got.UID != "urn:uuid:0e1" || got.URI != "ABC123.vcf" {
		t.Errorf("got = %+v", got)
	}
	// ...and by-uid finds it through the scan, since its uri is not <uid>.vcf.
	e.want(e.do(alice, "GET", "/api/v1/contacts/by-uid/urn:uuid:0e1", nil), 200)
	e.want(e.do(alice, "PUT", path(phoneID),
		map[string]string{"vcard": strings.Replace(phone, "Phone Named", "Phone Renamed", 1)},
		"If-Match", got.ETag), 200)
	e.want(e.do(alice, "PUT", path(phoneID),
		map[string]string{"vcard": vcf("some-other-uid", "Phone")}), http.StatusBadRequest)
	// A UID that could never be a file name (a space, 300 characters) is no
	// obstacle to editing a card that already has one.
	odd := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + strings.Repeat("x", 300) + " y\r\nFN:Odd UID\r\nEND:VCARD\r\n"
	if _, err := cardDAV.CreateCard(t.Context(), bookID, "odd.vcf", []byte(odd)); err != nil {
		t.Fatal(err)
	}
	e.want(e.do(alice, "PUT", path(e.idOf("odd.vcf")),
		map[string]string{"vcard": strings.Replace(odd, "Odd UID", "Odd Edited", 1)}), 200)
	// ...while a create with such a UID is still refused, since there the UID
	// becomes the file name.
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": odd}), http.StatusBadRequest)

	// Exactly one card per body: a second one would be stored inside the
	// first and served raw to every other client.
	e.want(e.do(alice, "POST", "/api/v1/contacts",
		map[string]string{"vcard": vcf("t1", "One") + vcf("t2", "Two")}), http.StatusBadRequest)
	e.want(e.do(alice, "GET", "/api/v1/contacts/by-uid/t1", nil), 404)
	// Nor anything else after END:VCARD, which the decoder would skip and
	// the store would keep.
	e.want(e.do(alice, "POST", "/api/v1/contacts",
		map[string]string{"vcard": vcf("t3", "Three") + "garbage\r\n"}), http.StatusBadRequest)
	e.want(e.do(alice, "POST", "/api/v1/contacts",
		map[string]string{"vcard": vcf("t4", "Four") + "\r\n\r\n"}), 201) // trailing blank lines are fine

	// A present-but-empty If-Match is a precondition that cannot hold, not an
	// unconditional write.
	e.want(e.do(alice, "PUT", path(phoneID),
		map[string]string{"vcard": strings.Replace(phone, "Phone Named", "Clobber", 1)},
		"If-Match", ""), http.StatusPreconditionFailed)
	e.want(e.do(alice, "GET", path(phoneID), nil), 200).json(t, &got)
	if !strings.Contains(got.VCard, "Phone Renamed") {
		t.Errorf("an empty If-Match wrote the card: %s", got.VCard)
	}

	// A legacy row with a NULL etag still hands the client a usable etag (the
	// md5 of its data) and a conditional update against it still works.
	if _, err := e.db.Exec(`UPDATE cards SET etag = NULL WHERE uri = 'ABC123.vcf'`); err != nil {
		t.Fatal(err)
	}
	e.want(e.do(alice, "GET", path(phoneID), nil), 200).json(t, &got)
	if len(got.ETag) != 32 {
		t.Fatalf("etag for a NULL-etag row = %q, want an md5", got.ETag)
	}
	var updated contactJSON
	e.want(e.do(alice, "PUT", path(phoneID),
		map[string]string{"vcard": strings.Replace(phone, "Phone Named", "Phone Final", 1)},
		"If-Match", `"`+got.ETag+`"`), 200).json(t, &updated)
	if updated.ETag == got.ETag || len(updated.ETag) != 32 {
		t.Errorf("etag after update = %q (was %q)", updated.ETag, got.ETag)
	}
}

func TestListSearchAndPaging(t *testing.T) {
	e := newEnv(t, "calserver_capi_search", contactsapi.Options{})
	e.principal("alice@example.org")
	const alice = "alice@example.org"
	for _, c := range []string{
		vcf("s1", "Zed Zimmer", "zed@example.net"),
		vcf("s2", "Bob Builder", "bob@example.net"),
		vcf("s3", "Anna Alpha", "anna@example.net", "a.alpha@work.example"),
		"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:s4\r\nN:Ünal;Şule;;;\r\nEMAIL:sule@example.net\r\nEND:VCARD\r\n",
	} {
		e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": c}), 201)
	}
	var list listJSON
	e.want(e.do(alice, "GET", "/api/v1/contacts", nil), 200).json(t, &list)
	if list.Total != 4 {
		t.Fatalf("total = %d, want 4", list.Total)
	}
	// Sorted by the lowercased display name, bytewise (so a leading non-ASCII
	// letter sorts after z — the same as a C-collation ORDER BY); a card with
	// no FN sorts by the name composed from N.
	var order []string
	for _, c := range list.Contacts {
		order = append(order, c.UID)
	}
	if strings.Join(order, ",") != "s3,s2,s1,s4" {
		t.Errorf("order = %v, want s3,s2,s1,s4", order)
	}
	// Paging: offset/limit slice the sorted list, total is the whole match.
	e.want(e.do(alice, "GET", "/api/v1/contacts?offset=1&limit=2", nil), 200).json(t, &list)
	if list.Total != 4 || len(list.Contacts) != 2 || list.Contacts[0].UID != "s2" || list.Contacts[1].UID != "s1" {
		t.Errorf("page = total %d %v", list.Total, list.Contacts)
	}
	// Search is a case-insensitive substring over name, N parts and every
	// email — so the work address finds Anna, and the given name finds Şule.
	for q, want := range map[string]string{"BOB": "s2", "work.example": "s3", "şule": "s4", "nobody": ""} {
		e.want(e.do(alice, "GET", "/api/v1/contacts?search="+q, nil), 200).json(t, &list)
		got := ""
		if len(list.Contacts) == 1 {
			got = list.Contacts[0].UID
		}
		if got != want || (want == "" && list.Total != 0) {
			t.Errorf("search %q: got %q (total %d), want %q", q, got, list.Total, want)
		}
	}
}

func TestSuggestionsAndFrecency(t *testing.T) {
	e := newEnv(t, "calserver_capi_suggest", contactsapi.Options{})
	e.principal("alice@example.org")
	const alice = "alice@example.org"
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("f1", "Card Only", "cardonly@example.net")}), 201)
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("f2", "Bob From Book", "bob@example.net")}), 201)

	// IncFrec: "Name <addr>" and bare forms, repeated, with an unparseable
	// entry that must not fail the batch.
	var bumped struct {
		Updated int `json:"updated"`
	}
	e.want(e.do(alice, "POST", "/api/v1/frecency", map[string][]string{
		"addresses": {"Bob Builder <Bob@Example.net>", "carol@example.net", "not an address"},
	}), 200).json(t, &bumped)
	if bumped.Updated != 2 {
		t.Errorf("updated = %d, want 2", bumped.Updated)
	}
	e.want(e.do(alice, "POST", "/api/v1/frecency", map[string][]string{"addresses": {"bob@example.net"}}), 200)

	// Frecency rows only, never a card.
	var list listJSON
	e.want(e.do(alice, "GET", "/api/v1/contacts", nil), 200).json(t, &list)
	if list.Total != 2 {
		t.Errorf("IncFrec created a card: total = %d, want 2", list.Total)
	}

	var sugg []struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	e.want(e.do(alice, "GET", "/api/v1/suggestions?q=", nil), 200).json(t, &sugg)
	// bob (frec 2) first; carol (frec 1, name from nowhere: parsed blank)
	// second; the card-only address last with the card's name. bob's name is
	// the card's, since a card knows better than a parsed header.
	if len(sugg) != 3 || sugg[0].Email != "bob@example.net" || sugg[0].Name != "Bob From Book" ||
		sugg[1].Email != "carol@example.net" || sugg[2].Email != "cardonly@example.net" || sugg[2].Name != "Card Only" {
		t.Errorf("suggestions = %+v", sugg)
	}
	e.want(e.do(alice, "GET", "/api/v1/suggestions?q=car", nil), 200).json(t, &sugg)
	if len(sugg) != 2 { // carol and Card Only
		t.Errorf("q=car: %+v", sugg)
	}

	// Above the scan guard the cards are not read: only the frecency table
	// answers, and the card-only address disappears from suggestions.
	guarded := contactsapi.New(e.db, fakeAuth{}, nil, contactsapi.Options{SuggestionScanLimit: 1})
	req := httptest.NewRequest("GET", "/api/v1/suggestions?q=", nil)
	req.SetBasicAuth(alice, "pw")
	w := httptest.NewRecorder()
	guarded.ServeHTTP(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &sugg); err != nil || len(sugg) != 2 {
		t.Errorf("guarded suggestions = %s (%v)", w.Body.String(), err)
	}
}

func TestDeleteAllIsScopedToTheWebmailBook(t *testing.T) {
	e := newEnv(t, "calserver_capi_deleteall", contactsapi.Options{})
	e.principal("alice@example.org")
	const alice = "alice@example.org"
	cardDAV := &storage.CardDAV{DB: e.db}
	// A second book the user made in calclient, with a card in it.
	otherID, err := cardDAV.CreateAddressBook(t.Context(), "principals/alice@example.org", "personal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cardDAV.CreateCard(t.Context(), otherID, "p1.vcf", []byte(vcf("p1", "Keep Me"))); err != nil {
		t.Fatal(err)
	}
	// With "personal" the sole book, a create would land there — so make the
	// webmail book explicitly first, as the migration does.
	webmailID, err := cardDAV.CreateAddressBook(t.Context(), "principals/alice@example.org", "webmail", nil)
	if err != nil {
		t.Fatal(err)
	}
	counters := func() (changes int, token int64) {
		t.Helper()
		if err := e.db.QueryRow(`SELECT
			(SELECT count(*) FROM addressbookchanges WHERE addressbookid = $1),
			(SELECT synctoken FROM addressbooks WHERE id = $1)`, webmailID).Scan(&changes, &token); err != nil {
			t.Fatal(err)
		}
		return
	}
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("w1", "One")}), 201)
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("w2", "Two")}), 201)

	var list listJSON
	e.want(e.do(alice, "GET", "/api/v1/contacts", nil), 200).json(t, &list)
	if list.Total != 3 {
		t.Fatalf("reads aggregate across books: total = %d, want 3", list.Total)
	}
	// A bare DELETE on the collection does nothing.
	e.want(e.do(alice, "DELETE", "/api/v1/contacts", nil), http.StatusBadRequest)

	var res struct {
		Deleted int    `json:"deleted"`
		Book    string `json:"book"`
	}
	changesBefore, tokenBefore := counters()
	e.want(e.do(alice, "DELETE", "/api/v1/contacts?all=1", nil), 200).json(t, &res)
	if res.Deleted != 2 || res.Book != "webmail" {
		t.Errorf("delete all = %+v, want 2 from webmail", res)
	}
	// One change row per deleted card, so a synced client learns of each, each
	// with its own token, so the token moved past both.
	changesAfter, tokenAfter := counters()
	if changesAfter-changesBefore != 2 || tokenAfter != tokenBefore+2 {
		t.Errorf("delete-all: change rows +%d (want 2), synctoken %d → %d (want +2)",
			changesAfter-changesBefore, tokenBefore, tokenAfter)
	}
	// An empty book: nothing deleted, nothing logged, token untouched.
	e.want(e.do(alice, "DELETE", "/api/v1/contacts?all=1", nil), 200).json(t, &res)
	if c, tok := counters(); res.Deleted != 0 || c != changesAfter || tok != tokenAfter {
		t.Errorf("delete-all on an empty book changed something: %+v, rows %d, token %d", res, c, tok)
	}
	e.want(e.do(alice, "GET", "/api/v1/contacts", nil), 200).json(t, &list)
	if list.Total != 1 || list.Contacts[0].URI != "p1.vcf" {
		t.Errorf("the other book was touched: %+v", list)
	}

	// Batch delete: one real, one missing, one malformed — ids are numbers,
	// the shape SnappyMail's DoContactsDelete hands a driver after intval().
	var w3 contactJSON
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("w3", "Three")}), 201).json(t, &w3)
	var batch struct {
		Deleted int     `json:"deleted"`
		Missing []int64 `json:"missing"`
	}
	e.want(e.do(alice, "POST", "/api/v1/contacts/batch-delete", map[string][]int64{
		"ids": {w3.ID, w3.ID + 1000, 0},
	}), 200).json(t, &batch)
	if batch.Deleted != 1 || len(batch.Missing) != 2 {
		t.Errorf("batch = %+v", batch)
	}
	// Delete-all with no webmail book and several others creates nothing.
	e.principal("dan@example.org")
	for _, uri := range []string{"a", "b"} {
		if _, err := cardDAV.CreateAddressBook(t.Context(), "principals/dan@example.org", uri, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.want(e.do("dan@example.org", "DELETE", "/api/v1/contacts?all=1", nil), 200).json(t, &res)
	var books int
	if err := e.db.QueryRow(`SELECT count(*) FROM addressbooks WHERE principaluri = 'principals/dan@example.org'`).Scan(&books); err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 || books != 2 {
		t.Errorf("delete-all conjured a book: %+v, books = %d", res, books)
	}
}
