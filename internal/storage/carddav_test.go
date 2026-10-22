package storage_test

// CardDAV backend regression tests against a scratch Postgres with the sabre
// schema. Postgres-only (no php/sabre needed), skips automatically when the
// dev Postgres isn't reachable, like the concurrency test.

import (
	"database/sql"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"calserver/internal/dav"
	"calserver/internal/storage"
)

// openCardDAVScratchDB creates a fresh database from sabre's schema files and
// opens it, mirroring the setup of TestAdminQueries/TestSyncTokenConcurrency.
func openCardDAVScratchDB(t *testing.T, dbName string) *sql.DB {
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

const testVCF = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:%s\r\nFN:%s\r\nEND:VCARD\r\n"

// TestChangesForAddressBookIncremental covers the bug where
// CardDAV.ChangesForAddressBook (internal/storage/carddav.go) only implements
// the initial sync (syncToken == "") and returns nil, nil for any non-empty
// valid token — which the sync REPORT (internal/dav/sync.go) maps to an
// invalid-sync-token 403. The calendar counterpart ChangesForCalendar
// (postgres.go) returns the incremental ChangeSet; the address book backend
// must do the same.
func TestChangesForAddressBookIncremental(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_abchanges")
	backend := &storage.CardDAV{DB: db}

	id, err := backend.CreateAddressBook(t.Context(), "principals/admin", "contacts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CreateCard(t.Context(), id, "card1.vcf", []byte(fmt.Sprintf(testVCF, "c1", "Card One"))); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CreateCard(t.Context(), id, "gone.vcf", []byte(fmt.Sprintf(testVCF, "c2", "Card Gone"))); err != nil {
		t.Fatal(err)
	}

	initial, err := backend.ChangesForAddressBook(t.Context(), id, "", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if initial == nil || len(initial.Added) != 2 {
		t.Fatalf("initial sync = %+v, want both cards added", initial)
	}

	// Changes after the initial token: one added, one modified, one deleted.
	if _, err := backend.CreateCard(t.Context(), id, "new.vcf", []byte(fmt.Sprintf(testVCF, "c3", "Card New"))); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.UpdateCard(t.Context(), id, "card1.vcf", []byte(fmt.Sprintf(testVCF, "c1", "Card One v2"))); err != nil {
		t.Fatal(err)
	}
	if err := backend.DeleteCard(t.Context(), id, "gone.vcf"); err != nil {
		t.Fatal(err)
	}

	cs, err := backend.ChangesForAddressBook(t.Context(), id, initial.SyncToken, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cs == nil {
		t.Fatalf("ChangesForAddressBook(%q) = nil, nil for a valid token — incremental sync unimplemented, clients get invalid-sync-token 403", initial.SyncToken)
	}
	if !reflect.DeepEqual(cs.Added, []string{"new.vcf"}) {
		t.Errorf("Added = %v, want [new.vcf]", cs.Added)
	}
	if !reflect.DeepEqual(cs.Modified, []string{"card1.vcf"}) {
		t.Errorf("Modified = %v, want [card1.vcf]", cs.Modified)
	}
	if !reflect.DeepEqual(cs.Deleted, []string{"gone.vcf"}) {
		t.Errorf("Deleted = %v, want [gone.vcf]", cs.Deleted)
	}
	oldToken, _ := strconv.ParseInt(initial.SyncToken, 10, 64)
	newToken, _ := strconv.ParseInt(cs.SyncToken, 10, 64)
	if newToken <= oldToken {
		t.Errorf("SyncToken = %q, want > %q", cs.SyncToken, initial.SyncToken)
	}
}

// TestUpdateAddressBookBumpsSyncToken covers the bug where
// CardDAV.UpdateAddressBook (internal/storage/carddav.go) updates the
// addressbooks columns without bumping the synctoken or recording a change
// row, unlike its calendar counterpart UpdateCalendar (postgres.go), which
// calls addChange(tx, id, "", 2). Sync clients therefore never see renames.
func TestUpdateAddressBookBumpsSyncToken(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_abupdate")
	backend := &storage.CardDAV{DB: db}

	id, err := backend.CreateAddressBook(t.Context(), "principals/admin", "contacts", nil)
	if err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := db.QueryRow(`SELECT synctoken FROM addressbooks WHERE id = $1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}

	pp := dav.NewPropPatch(map[xml.Name]any{
		{Space: "DAV:", Local: "displayname"}: "Renamed Contacts",
	})
	backend.UpdateAddressBook(t.Context(), id, pp)
	if result, ok := pp.Commit(); !ok {
		t.Fatalf("UpdateAddressBook proppatch failed: %v", result)
	}

	var name string
	var after int64
	if err := db.QueryRow(`SELECT displayname, synctoken FROM addressbooks WHERE id = $1`, id).Scan(&name, &after); err != nil {
		t.Fatal(err)
	}
	if name != "Renamed Contacts" {
		t.Fatalf("displayname = %q, want %q", name, "Renamed Contacts")
	}
	if after <= before {
		t.Errorf("synctoken = %d after rename, want > %d (UpdateAddressBook must bump the token like UpdateCalendar)", after, before)
	}
}

// TestSearchPrincipalsEscapesLike covers the bug where
// Principals.SearchPrincipals (internal/storage/postgres.go) builds LIKE
// patterns from the raw needle without escapeLike, so '_' and '%' in the
// search string act as wildcards and over-match unrelated principals.
func TestSearchPrincipalsEscapesLike(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_searchesc")
	root := repoRoot(t)
	run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", "calserver_searchesc", "-c",
		`INSERT INTO principals (uri, email, displayname) VALUES
			('principals/jdoe', 'jdoe@example.org', 'j_doe'),
			('principals/jxdoe', 'jxdoe@example.org', 'jxdoe')`)

	p := &storage.Principals{DB: db}
	got, err := p.SearchPrincipals(t.Context(), "principals",
		map[xml.Name]string{{Space: "DAV:", Local: "displayname"}: "j_doe"}, "anyof")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"principals/jdoe"}) {
		t.Errorf("SearchPrincipals(\"j_doe\") = %v, want [principals/jdoe] only — '_' must match literally, not as a LIKE wildcard", got)
	}
}

// TestDeleteAllCardsSyncsUnderALimit covers DeleteAllCards stamping every
// change row with one synctoken: a sync REPORT with a limit that cut through
// those rows resumed from one past that token and never reported the rest of
// the deletions.
func TestDeleteAllCardsSyncsUnderALimit(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_d_deleteall")
	backend := &storage.CardDAV{DB: db}

	id, err := backend.CreateAddressBook(t.Context(), "principals/admin", "contacts", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.vcf", "b.vcf", "c.vcf"}
	for _, uri := range want {
		if _, err := backend.CreateCard(t.Context(), id, uri, []byte(fmt.Sprintf(testVCF, uri, uri))); err != nil {
			t.Fatal(err)
		}
	}
	initial, err := backend.ChangesForAddressBook(t.Context(), id, "", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := backend.DeleteAllCards(t.Context(), id); err != nil || n != 3 {
		t.Fatalf("DeleteAllCards = %d, %v; want 3", n, err)
	}

	// Page through the changes one at a time, as a client with a limit does.
	var deleted []string
	token := initial.SyncToken
	for range 10 {
		cs, err := backend.ChangesForAddressBook(t.Context(), id, token, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		deleted = append(deleted, cs.Deleted...)
		token = cs.SyncToken
		if !cs.Truncated {
			break
		}
	}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("paged sync reported deletions %v, want %v", deleted, want)
	}
	// A later write must not reuse any of the deletions' tokens.
	if _, err := backend.CreateCard(t.Context(), id, "d.vcf", []byte(fmt.Sprintf(testVCF, "d", "d"))); err != nil {
		t.Fatal(err)
	}
	cs, err := backend.ChangesForAddressBook(t.Context(), id, token, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cs.Added, []string{"d.vcf"}) || len(cs.Deleted) != 0 {
		t.Fatalf("changes after the paged sync = %+v, want only d.vcf added", cs)
	}
}
