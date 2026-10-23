// Package storagetest creates scratch databases on the dev Postgres
// (scripts/dev-db.sh) for tests. Tests that use it skip when PGHOST is unset
// or the server does not answer.
package storagetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"calendsd/internal/pgdial"
	"calendsd/internal/schema"
)

// AdminSeed is the account sabre's example schema seeded and many tests
// assume: principals/admin with its proxy principals, and a users row with
// password "admin" in the SabreDAV realm. The migrations leave it out.
const AdminSeed = `
INSERT INTO principals (uri, email, displayname) VALUES
('principals/admin', 'admin@example.org', 'Administrator'),
('principals/admin/calendar-proxy-read', null, null),
('principals/admin/calendar-proxy-write', null, null);
INSERT INTO users (username, digesta1) VALUES ('admin', '87fd274b7b6c01e48d7c2f965da8ddf7');`

// DSN is the connection string for database name on the dev Postgres.
func DSN(name string) string {
	return fmt.Sprintf("postgres:///%s?host=%s", name, os.Getenv("PGHOST"))
}

// CreateDB drops database name if it exists, creates it empty, and returns
// it opened. It skips the test when the dev Postgres is not reachable.
func CreateDB(t testing.TB, name string) *sql.DB {
	t.Helper()
	if os.Getenv("PGHOST") == "" {
		t.Skip("skipping postgres test: PGHOST not set (run scripts/dev-db.sh start)")
	}
	admin, err := open(DSN("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("skipping postgres test: dev Postgres not reachable: %v", err)
	}
	for _, q := range []string{
		`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`,
		`CREATE DATABASE "` + name + `"`,
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db, err := open(DSN(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// NewDB is CreateDB with every migration applied and AdminSeed loaded.
func NewDB(t testing.TB, name string) *sql.DB {
	t.Helper()
	db := CreateDB(t, name)
	if _, err := schema.Apply(context.Background(), db, 0); err != nil {
		t.Fatalf("applying migrations to %s: %v", name, err)
	}
	if _, err := db.Exec(AdminSeed); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
	return db
}

// open connects the way storage.Open does, which this package cannot import:
// the jobs tests use it, and storage imports jobs.
func open(dsn string) (*sql.DB, error) {
	cfg, err := pgdial.Config(dsn)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*cfg), nil
}
