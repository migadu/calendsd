package storage_test

// Contacts-import regression tests against a scratch Postgres carrying both the
// sabre schema and calserver's extension tables. Postgres-only (no php/sabre
// needed), skipping automatically when the dev Postgres isn't reachable, like
// the CardDAV and concurrency tests.
//
// The subject is the re-run decision table in
// docs/snappymail-calserver-contacts.md: which of (ledger row, bundle digest,
// book synctoken) leads to a COPY, a diff, a skip, or a refusal. Those
// decisions are the difference between a re-run being something you do casually
// and a re-run duplicating 2.96M contacts, and none of them is visible from
// reading a row afterwards — a blind upsert produces correct rows too. So the
// assertions are mostly about what was NOT written.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calserver/internal/storage"
)

// openContactsScratchDB creates a fresh database from sabre's schema files plus
// calserver's extension schema (which is where contact_import and
// contact_frecency live) and opens it.
func openContactsScratchDB(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	root := repoRoot(t)
	if reason := pgOnlyPrereqsMissing(); reason != "" {
		t.Skipf("skipping postgres test: %s", reason)
	}
	run(t, root, "dropdb", "--if-exists", dbName)
	run(t, root, "createdb", dbName)
	sqlFiles, _ := filepath.Glob(filepath.Join(root, "dav/examples/sql/pgsql.*.sql"))
	if len(sqlFiles) == 0 {
		t.Fatal("no schema files found")
	}
	sqlFiles = append(sqlFiles, filepath.Join(root, "scripts/schema/pgsql.calserver.sql"))
	for _, f := range sqlFiles {
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-f", f)
	}
	db, err := storage.Open(fmt.Sprintf("postgres:///%s?host=%s", dbName, os.Getenv("PGHOST")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

const importVCF = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:%s\r\nFN:%s\r\nEND:VCARD\r\n"

func card(uid, fn string) storage.ImportCard {
	return storage.ImportCard{
		URI:          uid + ".vcf",
		Data:         []byte(fmt.Sprintf(importVCF, uid, fn)),
		LastModified: 1700000000,
	}
}

// digestOf stands in for the exporter's per-principal sentinel: the importer
// only ever compares it for equality, so the test needs a value that changes
// when the cards change and nothing more.
func digestOf(cards []storage.ImportCard) string {
	sum := ""
	for _, c := range cards {
		sum += c.URI + ":" + string(c.Data)
	}
	return fmt.Sprintf("%064x", len(sum))[:64]
}

func baseRequest(principal string, cards []storage.ImportCard) storage.ImportRequest {
	return storage.ImportRequest{
		RunID:       "11111111-1111-4111-8111-111111111111",
		Principal:   principal,
		SourceUsers: []int32{7, 9},
		BookURI:     "webmail",
		BookName:    "Webmail contacts",
		BookDesc:    "Contacts migrated from webmail",
		Digest:      digestOf(cards),
		ExporterVer: "snappymail-export-contacts/1 vobject/4.5.4",
		ExportEpoch: 1700000000,
		Cards:       cards,
	}
}

// counters is the state a re-run must not disturb: the change log a bulk
// re-write would flood, and the synctoken every DAV client keys on.
type counters struct {
	cards     int
	changes   int
	syncToken int64
}

func readCounters(t *testing.T, db *sql.DB, bookID int64) counters {
	t.Helper()
	var c counters
	if err := db.QueryRow(`SELECT
		(SELECT count(*) FROM cards WHERE addressbookid = $1),
		(SELECT count(*) FROM addressbookchanges WHERE addressbookid = $1),
		(SELECT synctoken FROM addressbooks WHERE id = $1)`, bookID).
		Scan(&c.cards, &c.changes, &c.syncToken); err != nil {
		t.Fatal(err)
	}
	return c
}

func mustImport(t *testing.T, imp *storage.ContactImport, req storage.ImportRequest) storage.ImportResult {
	t.Helper()
	res, err := imp.ImportPrincipal(context.Background(), req)
	if err != nil {
		t.Fatalf("ImportPrincipal(%s): %v", req.Principal, err)
	}
	return res
}

// TestContactImportFreshThenIdempotent is the property the whole design exists
// for: the first run creates everything, and a second run over the same bundle
// writes nothing at all — not a card, not a change row, not a synctoken bump.
func TestContactImportFreshThenIdempotent(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_ctimport")
	imp := &storage.ContactImport{DB: db}

	cards := []storage.ImportCard{card("c1", "Alice One"), card("c2", "Bob Two")}
	req := baseRequest("alice@example.org", cards)
	req.Frecency = []storage.SeedFrecency{
		// Over the column widths: the name is stored truncated to 255
		// characters, the "address" over 320 is dropped. Both came out of run
		// 1's real data, where junk FULLNAME rows reach 64 KB.
		{Email: "long@example.org", Name: strings.Repeat("ü", 300), Count: 1},
		{Email: strings.Repeat("x", 330) + "@example.org", Name: "Nobody", Count: 1},
		{Email: "Bob@Example.org", Name: "Bob", Count: 5},
		{Email: "bob@example.org", Name: "", Count: 3}, // same address, other case
	}

	first := mustImport(t, imp, req)
	if first.Action != storage.ActionImported {
		t.Fatalf("first run action = %q (%s), want imported", first.Action, first.Reason)
	}
	if !first.PrincipalCreated || !first.BookCreated {
		t.Errorf("first run: principalCreated=%v bookCreated=%v, want both true",
			first.PrincipalCreated, first.BookCreated)
	}
	if first.CardsCreated != 2 {
		t.Errorf("first run created %d cards, want 2", first.CardsCreated)
	}

	// The principal triple, not just the principal: without the proxy
	// sub-principals delegation breaks silently later.
	var principals int
	if err := db.QueryRow(
		`SELECT count(*) FROM principals WHERE uri LIKE 'principals/alice@example.org%'`).
		Scan(&principals); err != nil {
		t.Fatal(err)
	}
	if principals != 3 {
		t.Errorf("principal rows = %d, want 3 (principal + two calendar-proxy)", principals)
	}

	after := readCounters(t, db, first.BookID)
	if after.cards != 2 {
		t.Errorf("cards = %d, want 2", after.cards)
	}
	// A book this run created can hold no client's sync token, so there is
	// nothing to notify: the COPY writes no change rows and leaves the token
	// where CreateAddressBook set it. A client's first sync sends an empty
	// token and gets every card as Added.
	if after.changes != 0 {
		t.Errorf("change rows after a fresh import = %d, want 0", after.changes)
	}
	if after.syncToken != 1 {
		t.Errorf("synctoken after a fresh import = %d, want 1", after.syncToken)
	}

	// Frecency folded the two case-variant rows into one, keeping the name from
	// the higher count.
	var email, name string
	var seed int
	if err := db.QueryRow(
		`SELECT email, name, seed_frec FROM contact_frecency WHERE principal = $1 AND email = 'bob@example.org'`,
		"principals/alice@example.org").Scan(&email, &name, &seed); err != nil {
		t.Fatal(err)
	}
	if email != "bob@example.org" || name != "Bob" || seed != 8 {
		t.Errorf("frecency row = (%q, %q, %d), want (bob@example.org, Bob, 8)", email, name, seed)
	}
	// The over-long name landed truncated to the column width, in characters
	// not bytes; the over-long "address" was dropped, so two rows in all.
	var rows, nameLen int
	if err := db.QueryRow(
		`SELECT count(*), coalesce(max(length(name)) FILTER (WHERE email = 'long@example.org'), 0)
		   FROM contact_frecency WHERE principal = $1`,
		"principals/alice@example.org").Scan(&rows, &nameLen); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || nameLen != 255 {
		t.Errorf("frecency rows = %d (want 2), long name stored as %d chars (want 255)", rows, nameLen)
	}
	if first.FrecencyRows != 2 {
		t.Errorf("FrecencyRows = %d, want 2 (the over-long address is dropped, not stored)", first.FrecencyRows)
	}

	// Re-run, unchanged bundle.
	second := mustImport(t, imp, req)
	if second.Action != storage.ActionSkipped {
		t.Fatalf("second run action = %q (%s), want skipped", second.Action, second.Reason)
	}
	if got := readCounters(t, db, first.BookID); got != after {
		t.Errorf("second run changed the database: %+v, want %+v", got, after)
	}
	// And the frecency seed replaced rather than accumulated — a summing upsert
	// would read 16 here, which is the bug the seed_frec/frec split prevents.
	if err := db.QueryRow(
		`SELECT seed_frec FROM contact_frecency WHERE principal = $1`,
		"principals/alice@example.org").Scan(&seed); err != nil {
		t.Fatal(err)
	}
	if seed != 8 {
		t.Errorf("seed_frec after a re-run = %d, want 8 (it must replace, not accumulate)", seed)
	}
}

// TestContactImportDiffsOnlyWhatChanged covers the re-run case where the source
// moved: one card edited, one added, one untouched. The untouched card must not
// be rewritten, because a blind upsert of 2.96M cards is millions of change rows
// and a push notification per card telling every device to re-download
// everything.
func TestContactImportDiffsOnlyWhatChanged(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_ctimport_diff")
	imp := &storage.ContactImport{DB: db}

	first := mustImport(t, imp, baseRequest("bob@example.org",
		[]storage.ImportCard{card("c1", "One"), card("c2", "Two")}))
	if first.Action != storage.ActionImported {
		t.Fatalf("setup action = %q (%s)", first.Action, first.Reason)
	}

	changed := []storage.ImportCard{card("c1", "One"), card("c2", "Two Edited"), card("c3", "Three")}
	req := baseRequest("bob@example.org", changed)
	res := mustImport(t, imp, req)

	if res.Action != storage.ActionDiffed {
		t.Fatalf("action = %q (%s), want diffed", res.Action, res.Reason)
	}
	if res.CardsCreated != 1 || res.CardsUpdated != 1 || res.CardsSkipped != 1 {
		t.Errorf("created/updated/skipped = %d/%d/%d, want 1/1/1",
			res.CardsCreated, res.CardsUpdated, res.CardsSkipped)
	}
	// An ordinary diff reports counts, not a uri per changed card: that list is
	// reserved for --force-overwrite, where it names edits the bundle discarded.
	if len(res.Overwritten) != 0 {
		t.Errorf("overwritten = %v, want empty without --force-overwrite", res.Overwritten)
	}
	// Exactly two change rows: the add and the edit. A third would mean the
	// unchanged card was rewritten — the symptom of comparing the quoted etag
	// scanCard returns against a raw md5, which never matches.
	after := readCounters(t, db, first.BookID)
	if after.changes != 2 {
		t.Errorf("change rows = %d, want 2 (one add, one edit)", after.changes)
	}
	if after.cards != 3 {
		t.Errorf("cards = %d, want 3", after.cards)
	}
}

// TestContactImportNeverDeletes: a card present in calserver but absent from
// the bundle is reported as a delete candidate and left alone. The importer
// cannot tell "the user deleted this in calclient yesterday" from "this was
// tombstoned in SnappyMail after the export", and guessing wrong destroys data.
func TestContactImportNeverDeletes(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_ctimport_orphan")
	imp := &storage.ContactImport{DB: db}

	first := mustImport(t, imp, baseRequest("carol@example.org",
		[]storage.ImportCard{card("c1", "One"), card("c2", "Two")}))

	shrunk := baseRequest("carol@example.org", []storage.ImportCard{card("c1", "One")})
	res := mustImport(t, imp, shrunk)

	if res.Action != storage.ActionDiffed {
		t.Fatalf("action = %q (%s), want diffed", res.Action, res.Reason)
	}
	if len(res.Orphans) != 1 || res.Orphans[0] != "c2.vcf" {
		t.Errorf("orphans = %v, want [c2.vcf]", res.Orphans)
	}
	if got := readCounters(t, db, first.BookID); got.cards != 2 {
		t.Errorf("cards = %d, want 2 — the importer must not delete", got.cards)
	}
}

// TestContactImportLeavesOutCardsHeldElsewhere: a user who pointed
// SnappyMail's own CardDAV sync at this server already has their webmail
// contacts in a DAV book under the same UIDs. Importing those into "webmail"
// would show each of them twice on every phone, so the importer leaves them
// out, on the first run and on every run after it, and reports which.
func TestContactImportLeavesOutCardsHeldElsewhere(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_ctimport_elsewhere")
	imp := &storage.ContactImport{DB: db}
	cardDAV := &storage.CardDAV{DB: db}

	bookID, err := cardDAV.CreateAddressBook("principals/lena@example.org", "addressbook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cardDAV.CreateCard(bookID, "c2.vcf", []byte(fmt.Sprintf(importVCF, "c2", "Two From Sync"))); err != nil {
		t.Fatal(err)
	}

	cards := []storage.ImportCard{card("c1", "One"), card("c2", "Two"), card("c3", "Three")}
	first := mustImport(t, imp, baseRequest("lena@example.org", cards))
	if first.Action != storage.ActionImported {
		t.Fatalf("action = %q (%s), want imported", first.Action, first.Reason)
	}
	if first.CardsCreated != 2 || len(first.Elsewhere) != 1 || first.Elsewhere[0] != "c2.vcf" {
		t.Fatalf("created = %d, elsewhere = %v; want 2 and [c2.vcf]", first.CardsCreated, first.Elsewhere)
	}
	if got := readCounters(t, db, first.BookID); got.cards != 2 {
		t.Errorf("webmail cards = %d, want 2", got.cards)
	}
	// The user's own copy is untouched.
	if info, err := cardDAV.Card(bookID, "c2.vcf"); err != nil || !contains(string(info.CardData), "Two From Sync") {
		t.Errorf("the user's copy changed or vanished: %v", err)
	}

	// A changed bundle on a later run must not bring the twin either.
	changed := append([]storage.ImportCard{}, cards...)
	changed[0] = card("c1", "One Renamed")
	second := mustImport(t, imp, baseRequest("lena@example.org", changed))
	if second.Action != storage.ActionDiffed {
		t.Fatalf("second action = %q (%s), want diffed", second.Action, second.Reason)
	}
	if second.CardsCreated != 0 || second.CardsUpdated != 1 || len(second.Elsewhere) != 1 {
		t.Errorf("second run: created %d updated %d elsewhere %v; want 0, 1, [c2.vcf]",
			second.CardsCreated, second.CardsUpdated, second.Elsewhere)
	}
	if got := readCounters(t, db, first.BookID); got.cards != 2 {
		t.Errorf("webmail cards after second run = %d, want 2", got.cards)
	}
}

// TestContactImportRefusals walks the policy stops. Each is a reported result
// rather than an error, so one bad principal never ends a 110k-principal run.
func TestContactImportRefusals(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_ctimport_refuse")
	imp := &storage.ContactImport{DB: db}
	ctx := context.Background()

	t.Run("uppercase principal", func(t *testing.T) {
		// The login path builds principals/<lowercased email>. Creating
		// principals/User@Domain instead yields a second principal and an
		// address book nobody ever sees, with no error anywhere.
		res := mustImport(t, imp, baseRequest("Dave@Example.org", []storage.ImportCard{card("c1", "One")}))
		if res.Action != storage.ActionRefused {
			t.Fatalf("action = %q, want refused", res.Action)
		}
	})

	t.Run("master login form", func(t *testing.T) {
		// rcptd accepts user@domain@MASTER and calserver forwards any string,
		// so an unguarded import would mint a junk principal from a submission
		// credential.
		res := mustImport(t, imp, baseRequest("dave@example.org@master", []storage.ImportCard{card("c1", "One")}))
		if res.Action != storage.ActionRefused {
			t.Fatalf("action = %q, want refused", res.Action)
		}
	})

	t.Run("unsafe card uri", func(t *testing.T) {
		// The uri becomes a DAV path segment; a slash makes the card
		// unreachable from every client. The exporter derives a uid for
		// these, and the importer must not trust that it did.
		for _, uid := range []string{"a/b", "a b", "a\tb", ".hidden", "a\\b"} {
			res := mustImport(t, imp, baseRequest("carol@example.org", []storage.ImportCard{card(uid, "One")}))
			if res.Action != storage.ActionRefused {
				t.Errorf("uid %q: action = %q, want refused", uid, res.Action)
			}
		}
		for _, uid := range []string{"urn:uuid:1-2", "ab-12@host.example", "x=y+z"} {
			res := mustImport(t, imp, baseRequest("carol@example.org", []storage.ImportCard{card(uid, "One")}))
			if res.Action == storage.ActionRefused {
				t.Errorf("uid %q: refused (%s), want accepted", uid, res.Reason)
			}
		}
	})

	t.Run("duplicate card uri", func(t *testing.T) {
		res := mustImport(t, imp, baseRequest("dup@example.org",
			[]storage.ImportCard{card("c1", "One"), card("c1", "One Again")}))
		if res.Action != storage.ActionRefused {
			t.Fatalf("action = %q, want refused", res.Action)
		}
	})

	t.Run("tombstoned principal", func(t *testing.T) {
		writes := &storage.AdminWrites{DB: db}
		if err := writes.CreateUser("erin@example.org", storage.BlockedDigestA1(), "erin@example.org", "Erin"); err != nil {
			t.Fatal(err)
		}
		if err := writes.ScheduleDelete("erin@example.org", 30, "test"); err != nil {
			t.Fatal(err)
		}
		res := mustImport(t, imp, baseRequest("erin@example.org", []storage.ImportCard{card("c1", "One")}))
		if res.Action != storage.ActionRefused {
			t.Fatalf("action = %q (%s), want refused", res.Action, res.Reason)
		}
		// Importing into an account pending purge either resurrects contacts
		// scheduled for destruction or blocks the purge; neither is wanted, and
		// the operator has to see the list.
		var books int
		if err := db.QueryRow(`SELECT count(*) FROM addressbooks WHERE principaluri = $1`,
			"principals/erin@example.org").Scan(&books); err != nil {
			t.Fatal(err)
		}
		if books != 0 {
			t.Errorf("address books created for a tombstoned principal = %d, want 0", books)
		}
	})

	t.Run("foreign webmail book", func(t *testing.T) {
		// A calclient user who happened to name a book "webmail". With no
		// ledger row there is nothing to prove the book is ours, so merging
		// into it is refused rather than attempted.
		cardDAV := &storage.CardDAV{DB: db}
		if _, err := cardDAV.CreateAddressBook("principals/frank@example.org", "webmail", nil); err != nil {
			t.Fatal(err)
		}
		res := mustImport(t, imp, baseRequest("frank@example.org", []storage.ImportCard{card("c1", "One")}))
		if res.Action != storage.ActionRefused {
			t.Fatalf("action = %q (%s), want refused", res.Action, res.Reason)
		}
	})

	t.Run("book uri mismatch", func(t *testing.T) {
		// The ledger names the book this principal's cards live in. A later run
		// with a different --book-uri must not write into the ledger's book and
		// then record the new name over it.
		req := baseRequest("ivan@example.org", []storage.ImportCard{card("c1", "One")})
		mustImport(t, imp, req)
		renamed := req
		renamed.BookURI = "contacts"
		res := mustImport(t, imp, renamed)
		if res.Action != storage.ActionRefused {
			t.Fatalf("action = %q (%s), want refused", res.Action, res.Reason)
		}
	})

	t.Run("exporter version drift", func(t *testing.T) {
		req := baseRequest("grace@example.org", []storage.ImportCard{card("c1", "One")})
		mustImport(t, imp, req)

		upgraded := req
		upgraded.ExporterVer = "snappymail-export-contacts/1 vobject/4.6.0"
		upgraded.Cards = []storage.ImportCard{card("c1", "One Edited")}
		upgraded.Digest = digestOf(upgraded.Cards)

		// A serializer change makes every digest differ for reasons that are
		// not data; accepting it silently would rewrite every card in the
		// corpus with a change row and a push each.
		res, err := imp.ImportPrincipal(ctx, upgraded)
		if err != nil {
			t.Fatal(err)
		}
		if res.Action != storage.ActionRefused {
			t.Fatalf("action = %q, want refused", res.Action)
		}

		upgraded.ForceVersion = true
		res = mustImport(t, imp, upgraded)
		if res.Action != storage.ActionDiffed {
			t.Fatalf("action with --force-version = %q (%s), want diffed", res.Action, res.Reason)
		}
	})
}

// TestContactImportSyncTokenMoved covers the two rows of the decision table
// that turn on the destination having changed since the last run — a DAV client
// editing the book the import created.
func TestContactImportSyncTokenMoved(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_ctimport_token")
	imp := &storage.ContactImport{DB: db}
	cardDAV := &storage.CardDAV{DB: db}

	cards := []storage.ImportCard{card("c1", "One"), card("c2", "Two"), card("c3", "Three")}
	first := mustImport(t, imp, baseRequest("heidi@example.org", cards))
	bookID := fmt.Sprint(first.BookID)

	// A phone edits a card: synctoken moves, source unchanged.
	if _, err := cardDAV.UpdateCard(bookID, "c1.vcf", []byte(fmt.Sprintf(importVCF, "c1", "One From Phone"))); err != nil {
		t.Fatal(err)
	}

	same := mustImport(t, imp, baseRequest("heidi@example.org", cards))
	if same.Action != storage.ActionTokenRefreshed {
		t.Fatalf("action = %q (%s), want synctoken-refreshed", same.Action, same.Reason)
	}
	// The phone's edit survives: an unchanged source has no business
	// overwriting it.
	var fn string
	if err := db.QueryRow(
		`SELECT encode(carddata, 'escape') FROM cards WHERE addressbookid = $1 AND uri = 'c1.vcf'`,
		first.BookID).Scan(&fn); err != nil {
		t.Fatal(err)
	}
	if want := "One From Phone"; !contains(fn, want) {
		t.Errorf("card was overwritten; want it to still contain %q", want)
	}

	// The refresh above recorded where the token got to, so as far as the
	// ledger is concerned the destination is settled again. Move it once more,
	// so the next run really is "both sides changed since I last looked".
	if _, err := cardDAV.UpdateCard(bookID, "c2.vcf", []byte(fmt.Sprintf(importVCF, "c2", "Two From Phone"))); err != nil {
		t.Fatal(err)
	}
	// ...and deletes one. The bundle still carries it.
	if err := cardDAV.DeleteCard(bookID, "c3.vcf"); err != nil {
		t.Fatal(err)
	}

	// Now the source changes too — the one case that needs an explicit
	// decision, because the bundle and a client both moved.
	changed := []storage.ImportCard{card("c1", "One Edited"), card("c2", "Two"), card("c3", "Three")}
	req := baseRequest("heidi@example.org", changed)
	res := mustImport(t, imp, req)
	if res.Action != storage.ActionRefused {
		t.Fatalf("action = %q (%s), want refused without --force-overwrite", res.Action, res.Reason)
	}

	req.ForceOverwrite = true
	res = mustImport(t, imp, req)
	if res.Action != storage.ActionDiffed {
		t.Fatalf("action with --force-overwrite = %q (%s), want diffed", res.Action, res.Reason)
	}
	// Both cards: c1 because the bundle edited it, c2 because the phone did and
	// the bundle won. The list is populated only under --force-overwrite, which
	// is the case where the operator needs to know whose phone edits were
	// discarded; an ordinary diff reports counts alone.
	if len(res.Overwritten) != 2 {
		t.Errorf("overwritten = %v, want both cards", res.Overwritten)
	}
	// c3 came back: the phone deleted it, the bundle still had it, and under
	// --force-overwrite the bundle wins. The importer cannot tell that from a
	// contact newly added in webmail, so it names the uri and lets the
	// operator decide.
	if len(res.Created) != 1 || res.Created[0] != "c3.vcf" {
		t.Errorf("created = %v, want [c3.vcf]", res.Created)
	}

	// The ledger's cards_written accumulates: the first run wrote three, this
	// one wrote three more (c1 and c2 updated, c3 recreated).
	var written int
	if err := db.QueryRow(`SELECT cards_written FROM contact_import WHERE principal = $1`,
		"principals/heidi@example.org").Scan(&written); err != nil {
		t.Fatal(err)
	}
	if written != 6 {
		t.Errorf("ledger cards_written = %d, want 6 (cumulative across runs)", written)
	}
}

// TestContactImportDryRun: --dry-run decides everything and writes nothing, so
// running it against the full bundle before every real run costs only time.
func TestContactImportDryRun(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_ctimport_dry")
	imp := &storage.ContactImport{DB: db}

	req := baseRequest("ivan@example.org", []storage.ImportCard{card("c1", "One")})
	req.DryRun = true
	res := mustImport(t, imp, req)

	if res.Action != storage.ActionImported {
		t.Fatalf("action = %q (%s), want imported (the decision it would have made)", res.Action, res.Reason)
	}
	for table, query := range map[string]string{
		"principals":     `SELECT count(*) FROM principals WHERE uri = 'principals/ivan@example.org'`,
		"addressbooks":   `SELECT count(*) FROM addressbooks WHERE principaluri = 'principals/ivan@example.org'`,
		"contact_import": `SELECT count(*) FROM contact_import WHERE principal = 'principals/ivan@example.org'`,
	} {
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("--dry-run left %d row(s) in %s, want 0", n, table)
		}
	}
}

// TestCopyIntoInsideTx pins the driver behaviour copyInto rests on, which is
// not documented by database/sql and would otherwise be an assumption: a COPY
// sent through Conn.Raw lands inside the transaction the *sql.Tx from that same
// *sql.Conn has open, and rolls back with it. If this ever stops holding, the
// importer would commit cards outside its per-principal transaction — silently.
func TestCopyIntoInsideTx(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_ctimport_copy")
	imp := &storage.ContactImport{DB: db}

	req := baseRequest("judy@example.org", []storage.ImportCard{card("c1", "One"), card("c2", "Two")})
	req.DryRun = true // commits nothing: the rollback must take the COPY with it
	if res := mustImport(t, imp, req); res.CardsCreated != 2 {
		t.Fatalf("dry run reported %d cards created, want 2", res.CardsCreated)
	}
	var cards int
	if err := db.QueryRow(`SELECT count(*) FROM cards`).Scan(&cards); err != nil {
		t.Fatal(err)
	}
	if cards != 0 {
		t.Fatalf("%d card(s) survived the rollback — COPY ran outside the transaction", cards)
	}
}

// TestContactImportLocksTheBookWhileComparing covers the re-run reading the
// book's synctoken without a lock: a DAV client could then write between the
// read and the diff, and the import would overwrite that edit believing the
// book untouched since the last run. With a client transaction holding the
// book row, the import must wait rather than read past it.
func TestContactImportLocksTheBookWhileComparing(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_d_ctimport_lock")
	imp := &storage.ContactImport{DB: db}
	cards := []storage.ImportCard{card("a", "A")}
	first := mustImport(t, imp, baseRequest("lock@example.org", cards))

	// A client write in flight holds the book row, as abAddChange does.
	client, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Rollback()
	if _, err := client.Exec(
		`SELECT synctoken FROM addressbooks WHERE id = $1 FOR UPDATE`, first.BookID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if res, err := imp.ImportPrincipal(ctx, baseRequest("lock@example.org", cards)); err == nil {
		t.Fatalf("re-run went ahead (%s) while a client held the book; want it to wait", res.Action)
	}

	if err := client.Rollback(); err != nil {
		t.Fatal(err)
	}
	if res := mustImport(t, imp, baseRequest("lock@example.org", cards)); res.Action != storage.ActionSkipped {
		t.Fatalf("re-run after the client finished = %s, want skipped", res.Action)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
