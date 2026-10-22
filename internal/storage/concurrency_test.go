package storage_test

// Sync-token serialization test (PLAN.md §5): many writers hammering one
// calendar must produce unique, contiguous sync-tokens. A lost-update on the
// synctoken counter — the race the row lock in recordChange prevents —
// would duplicate a token and skip a value, silently losing a change from a
// client's sync. Postgres-only (no php/sabre needed):
//
//	go test ./internal/storage/ -run TestSyncTokenConcurrency
//
// Skips automatically when the dev Postgres isn't reachable.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"calendsd/internal/caldav"
	"calendsd/internal/storage"
)

func TestSyncTokenConcurrency(t *testing.T) {
	root := repoRoot(t)
	if reason := pgOnlyPrereqsMissing(); reason != "" {
		t.Skipf("skipping sync-token concurrency test: %s", reason)
	}

	const dbName = "calserver_concurrency"
	run(t, root, "dropdb", "--if-exists", dbName)
	run(t, root, "createdb", dbName)
	sqlFiles, _ := filepath.Glob(filepath.Join(root, "dav/examples/sql/pgsql.*.sql"))
	if len(sqlFiles) == 0 {
		t.Fatal("no schema files found")
	}
	for _, f := range sqlFiles {
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-f", f)
	}
	run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-c",
		`INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT');
		 INSERT INTO calendarinstances (calendarid, principaluri, access, uri)
		 VALUES (currval('calendars_id_seq'), 'principals/admin', 1, 'default')`)

	db, err := storage.Open(fmt.Sprintf("postgres:///%s?host=%s", dbName, os.Getenv("PGHOST")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var calID int64
	if err := db.QueryRow(`SELECT id FROM calendars LIMIT 1`).Scan(&calID); err != nil {
		t.Fatal(err)
	}
	backend := &storage.CalDAV{DB: db}
	id := caldav.CalendarID{Calendar: strconv.FormatInt(calID, 10)}

	const writers = 20
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all writers at once to maximize contention
			ics := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:c-%d\r\nDTSTART:20260101T120000Z\r\nDTEND:20260101T130000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", i)
			_, errs[i] = backend.CreateCalendarObject(t.Context(), id, fmt.Sprintf("obj%d.ics", i), []byte(ics))
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("writer %d failed: %v", i, e)
		}
	}

	// Every change must have a unique, contiguous synctoken 1..writers.
	rows, err := db.Query(`SELECT synctoken FROM calendarchanges WHERE calendarid = $1 ORDER BY synctoken`, calID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tokens []int64
	for rows.Next() {
		var tk int64
		if err := rows.Scan(&tk); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tk)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(tokens) != writers {
		t.Fatalf("change rows = %d, want %d", len(tokens), writers)
	}
	for i, tk := range tokens {
		if want := int64(1 + i); tk != want {
			t.Fatalf("change %d has synctoken %d, want %d — a duplicate/skipped token means the FOR UPDATE serialization is broken", i, tk, want)
		}
	}

	var calToken int64
	if err := db.QueryRow(`SELECT synctoken FROM calendars WHERE id = $1`, calID).Scan(&calToken); err != nil {
		t.Fatal(err)
	}
	if want := int64(1 + writers); calToken != want {
		t.Fatalf("calendar synctoken = %d, want %d", calToken, want)
	}
}

// pgOnlyPrereqsMissing reports why the Postgres-only test can't run: it needs
// the psql tooling and the dev Postgres, but not php/sabre.
func pgOnlyPrereqsMissing() string {
	for _, bin := range []string{"psql", "createdb", "dropdb"} {
		if _, err := exec.LookPath(bin); err != nil {
			return bin + " not on PATH"
		}
	}
	if os.Getenv("PGHOST") == "" {
		return "PGHOST not set"
	}
	if err := exec.Command("pg_isready", "-h", os.Getenv("PGHOST")).Run(); err != nil {
		return "postgres not reachable (run scripts/dev-db.sh start)"
	}
	return ""
}
