package storage_test

// Indexed lookups (lookups.go) against a scratch Postgres with the sabre
// schema plus the calserver extensions. Postgres-only; skips when the dev
// Postgres isn't reachable.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"calserver/internal/storage"
)

// lookupsDB builds a scratch database with the full schema, runs seed, and
// opens it. The database is dropped when the test ends.
func lookupsDB(t *testing.T, dbName, seed string) *sql.DB {
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
	return db
}

func TestLookupsPrincipalByAddress(t *testing.T) {
	principals := &storage.Principals{DB: lookupsDB(t, "calserver_b_lookups_address", `
		INSERT INTO principals (uri, email, displayname) VALUES
			('principals/alice', 'Alice@Example.org', 'Alice'),
			('principals/alice/calendar-proxy-read', 'proxy@example.org', NULL),
			('principals/under_score', 'u@example.org', NULL),
			('other/bob', 'bob@example.org', 'Bob');
	`)}
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
		got, err := principals.PrincipalByAddress(t.Context(), tc.prefix, tc.address)
		if err != nil {
			t.Fatalf("PrincipalByAddress(%q, %q): %v", tc.prefix, tc.address, err)
		}
		if got != tc.want {
			t.Errorf("PrincipalByAddress(%q, %q) = %q, want %q", tc.prefix, tc.address, got, tc.want)
		}
	}
}

func TestLookupsCalendarObjectByUID(t *testing.T) {
	backend := &storage.CalDAV{DB: lookupsDB(t, "calserver_b_lookups_uid", `
		INSERT INTO principals (uri, email) VALUES ('principals/alice', 'alice@example.org');
		INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT');
		INSERT INTO calendarinstances (calendarid, principaluri, access, uri)
			VALUES (currval('calendars_id_seq'), 'principals/alice', 1, 'a');
		INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT');
		INSERT INTO calendarinstances (calendarid, principaluri, access, uri)
			VALUES (currval('calendars_id_seq'), 'principals/alice', 1, 'b');
	`)}
	cals, err := backend.CalendarsForUser(t.Context(), "principals/alice")
	if err != nil || len(cals) != 2 {
		t.Fatalf("calendars = %d (%v), want 2", len(cals), err)
	}
	event := func(uid string) []byte {
		return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:" + uid +
			"\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260301T100000Z\r\nDTEND:20260301T110000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	}
	for _, o := range []struct {
		cal      int
		uri, uid string
	}{{0, "one.ics", "uid-1"}, {0, "two.ics", "uid-2"}, {1, "other.ics", "uid-1"}} {
		if _, err := backend.CreateCalendarObject(t.Context(), cals[o.cal].ID, o.uri, event(o.uid)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		cal       int
		uid, want string
	}{{0, "uid-1", "one.ics"}, {0, "uid-2", "two.ics"}, {1, "uid-1", "other.ics"}, {1, "uid-2", ""}, {0, "missing", ""}} {
		obj, err := backend.CalendarObjectByUID(t.Context(), cals[tc.cal].ID, tc.uid)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if obj != nil {
			got = obj.URI
			if !strings.Contains(string(obj.CalendarData), "UID:"+tc.uid) {
				t.Errorf("object %s carries the wrong data", got)
			}
		}
		if got != tc.want {
			t.Errorf("CalendarObjectByUID(cal %d, %q) = %q, want %q", tc.cal, tc.uid, got, tc.want)
		}
	}
}
