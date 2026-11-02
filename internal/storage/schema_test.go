package storage_test

// schema.Apply against scratch Postgres databases: a fresh one, one
// built the way the deploy built them before the runner, two runs at once,
// and a run behind a held lock. Skips when the dev Postgres isn't reachable.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/calendsd/internal/schema"
	"github.com/migadu/calendsd/internal/storage"
	"github.com/migadu/calendsd/internal/storage/storagetest"
)

func TestApplyFresh(t *testing.T) {
	db := storagetest.CreateDB(t, "calserver_schema_fresh")
	q := &storage.AdminQueries{DB: db}
	ctx := t.Context()

	if v, err := schema.Version(ctx, db); err != nil || v != 0 {
		t.Fatalf("Version on an empty database = %d, %v; want 0", v, err)
	}
	if err := q.SchemaPresent(ctx); err == nil {
		t.Fatal("SchemaPresent on an empty database succeeded")
	}
	applied, err := schema.Apply(ctx, db, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != schema.Latest() {
		t.Errorf("first run applied %d migrations, want %d", len(applied), schema.Latest())
	}
	if v, err := schema.Version(ctx, db); err != nil || v != schema.Latest() {
		t.Errorf("Version = %d, %v; want %d", v, err, schema.Latest())
	}
	if err := q.SchemaPresent(ctx); err != nil {
		t.Errorf("SchemaPresent after ApplyMigrations: %v", err)
	}
	var users int
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil || users != 0 {
		t.Errorf("users after migrating = %d, %v; want no seeded account", users, err)
	}
	if applied, err := schema.Apply(ctx, db, 5*time.Second); err != nil || len(applied) != 0 {
		t.Errorf("second run applied %v, %v; want nothing", applied, err)
	}
}

// A database from before the runner has every baseline object but no
// schema_migrations. The baseline runs over it as a no-op and is recorded.
func TestApplyOverExistingSchema(t *testing.T) {
	db := storagetest.CreateDB(t, "calserver_schema_existing")
	for _, m := range schema.Migrations()[:2] {
		mustExec(t, db, m.SQL)
	}
	mustExec(t, db, storagetest.AdminSeed)

	applied, err := schema.Apply(t.Context(), db, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != schema.Latest() {
		t.Errorf("applied %d migrations, want %d", len(applied), schema.Latest())
	}
	var users int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE username = 'admin'`).Scan(&users); err != nil || users != 1 {
		t.Errorf("admin users row after migrating = %d, %v; want it kept", users, err)
	}
}

func TestApplyConcurrently(t *testing.T) {
	db := storagetest.CreateDB(t, "calserver_schema_concurrent")
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	for range 2 {
		wg.Go(func() {
			applied, err := schema.Apply(t.Context(), db, 0)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += len(applied)
			mu.Unlock()
		})
	}
	wg.Wait()
	if total != schema.Latest() {
		t.Errorf("two runs applied %d migrations in all, want each of %d once", total, schema.Latest())
	}
}

// A lock held elsewhere makes the run fail within the lock timeout, not
// queue behind it while holding the locks it already took, and the
// migration it was in is not recorded.
func TestApplyLockTimeout(t *testing.T) {
	db := storagetest.NewDB(t, "calserver_schema_lock")
	mustExec(t, db, `DELETE FROM schema_migrations WHERE version = 2`)

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
	if _, err := schema.Apply(ctx, db, 200*time.Millisecond); err == nil || !strings.Contains(err.Error(), "55P03") {
		t.Fatalf("Apply behind a held lock = %v, want a lock timeout (55P03)", err)
	}
	tx.Rollback()
	var recorded bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 2)`).Scan(&recorded); err != nil || recorded {
		t.Errorf("version 2 recorded after its run failed = %v, %v", recorded, err)
	}
}
