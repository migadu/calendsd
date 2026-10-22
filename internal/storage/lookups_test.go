package storage_test

// Indexed lookups (lookups.go) against a scratch Postgres with the sabre
// schema plus the calserver extensions. Postgres-only; skips when the dev
// Postgres isn't reachable.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"calserver/internal/storage"
)

// lookupsDB builds a scratch database with the full schema, runs seed, and
// opens it. The database is dropped when the test ends.
func lookupsDB(t *testing.T, dbName, seed string) *storage.Principals {
	t.Helper()
	root := repoRoot(t)
	if reason := pgOnlyPrereqsMissing(); reason != "" {
		t.Skipf("skipping %s: %s", t.Name(), reason)
	}
	run(t, root, "dropdb", "--if-exists", dbName)
	run(t, root, "createdb", dbName)
	t.Cleanup(func() { run(t, root, "dropdb", "--if-exists", dbName) })
	sqlFiles, _ := filepath.Glob(filepath.Join(root, "dav/examples/sql/pgsql.*.sql"))
	extFiles, _ := filepath.Glob(filepath.Join(root, "scripts/schema/pgsql.*.sql"))
	if len(sqlFiles) == 0 || len(extFiles) == 0 {
		t.Fatalf("schema files missing (sabre=%d, ext=%d)", len(sqlFiles), len(extFiles))
	}
	for _, f := range append(sqlFiles, extFiles...) {
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-f", f)
	}
	run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-c", seed)

	db, err := storage.Open(fmt.Sprintf("postgres:///%s?host=%s", dbName, os.Getenv("PGHOST")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &storage.Principals{DB: db}
}

func TestLookupsPrincipalByAddress(t *testing.T) {
	principals := lookupsDB(t, "calserver_b_lookups_address", `
		INSERT INTO principals (uri, email, displayname) VALUES
			('principals/alice', 'Alice@Example.org', 'Alice'),
			('principals/alice/calendar-proxy-read', 'proxy@example.org', NULL),
			('principals/under_score', 'u@example.org', NULL),
			('other/bob', 'bob@example.org', 'Bob');
	`)
	for _, tc := range []struct{ prefix, address, want string }{
		{"principals", "mailto:alice@example.org", "principals/alice"},
		{"principals", "MAILTO:ALICE@EXAMPLE.ORG", "principals/alice"},
		{"principals", "mailto:u@example.org", "principals/under_score"},
		// Only direct children of the prefix, like the principal listing.
		{"principals", "mailto:proxy@example.org", ""},
		{"principals", "mailto:bob@example.org", ""},
		{"other", "mailto:bob@example.org", "other/bob"},
		// Only mailto: addresses carry an email.
		{"principals", "alice@example.org", ""},
		{"principals", "mailto:", ""},
		{"principals", "mailto:nobody@example.org", ""},
	} {
		got, err := principals.PrincipalByAddress(tc.prefix, tc.address)
		if err != nil {
			t.Fatalf("PrincipalByAddress(%q, %q): %v", tc.prefix, tc.address, err)
		}
		if got != tc.want {
			t.Errorf("PrincipalByAddress(%q, %q) = %q, want %q", tc.prefix, tc.address, got, tc.want)
		}
	}
}
