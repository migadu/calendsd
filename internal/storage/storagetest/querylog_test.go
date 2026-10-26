package storagetest_test

import (
	"slices"
	"testing"

	"calendsd/internal/storage/storagetest"
)

// The query log sees every statement a test runs through the logged handle,
// in order, whether directly or inside a transaction, and nothing from the
// migrations and seed that built the database.
func TestQueryLogRecordsStatements(t *testing.T) {
	db, log := storagetest.NewLoggedDB(t, "calserver_b_querylog")
	if n := log.Count(); n != 0 {
		t.Fatalf("fresh log has %d statements: %q", n, log.Statements())
	}

	if _, err := db.Exec(`SELECT 1`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM principals`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT 2`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	want := []string{`SELECT 1`, `SELECT count(*) FROM principals`, `SELECT 2`}
	if got := log.Statements(); !slices.Equal(got, want) {
		t.Errorf("statements = %q, want %q", got, want)
	}

	log.Reset()
	if n := log.Count(); n != 0 {
		t.Errorf("after Reset: %d statements", n)
	}
}
