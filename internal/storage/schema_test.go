package storage_test

// ApplySchema against a scratch Postgres: the embedded file loads over a bare
// sabre schema, loads again as a no-op, and SchemaPresent tells the two
// states apart. Skips when the dev Postgres isn't reachable.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendsd/internal/storage"
)

func TestApplySchema(t *testing.T) {
	root := repoRoot(t)
	if reason := pgOnlyPrereqsMissing(); reason != "" {
		t.Skipf("skipping schema test: %s", reason)
	}

	const dbName = "calserver_schema"
	run(t, root, "dropdb", "--if-exists", dbName)
	run(t, root, "createdb", dbName)
	sqlFiles, _ := filepath.Glob(filepath.Join(root, "dav/examples/sql/pgsql.*.sql"))
	for _, f := range sqlFiles {
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-f", f)
	}

	db, err := storage.Open(fmt.Sprintf("postgres:///%s?host=%s", dbName, os.Getenv("PGHOST")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := &storage.AdminQueries{DB: db}

	if err := q.SchemaPresent(t.Context()); err == nil || !strings.Contains(err.Error(), "notifications") {
		t.Fatalf("SchemaPresent on a sabre-only database = %v, want the first extension table missing", err)
	}
	for i := range 2 {
		if err := storage.ApplySchema(context.Background(), db, 5*time.Second); err != nil {
			t.Fatalf("ApplySchema run %d: %v", i+1, err)
		}
	}
	if err := q.SchemaPresent(t.Context()); err != nil {
		t.Fatalf("SchemaPresent after ApplySchema: %v", err)
	}

	// A lock held elsewhere makes the run fail within the lock timeout, not
	// queue behind it while holding the locks it already took.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("LOCK TABLE calendar_availability IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := storage.ApplySchema(ctx, db, 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "55P03") {
		t.Fatalf("ApplySchema behind a held lock = %v, want a lock timeout (55P03)", err)
	}
}
