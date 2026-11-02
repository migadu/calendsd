package caldav_test

// Postgres-backed test for AdvisoryLocker: two instances sharing one database
// stand in for two calendsd nodes, and must not both hold the per-object
// write-back lock at once. Skips when the dev Postgres isn't reachable.

import (
	"database/sql"
	"os"
	"os/exec"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/migadu/calendsd/internal/caldav"
)

func openLockDB(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	host := os.Getenv("PGHOST")
	if host == "" {
		t.Skip("skipping: PGHOST not set (dev Postgres not running)")
	}
	if err := exec.Command("pg_isready", "-h", host).Run(); err != nil {
		t.Skipf("skipping: dev Postgres not reachable: %v", err)
	}
	_ = exec.Command("dropdb", "--if-exists", dbName).Run()
	if out, err := exec.Command("createdb", dbName).CombinedOutput(); err != nil {
		t.Fatalf("createdb: %v\n%s", err, out)
	}
	db, err := sql.Open("pgx", "postgres:///"+dbName+"?host="+host)
	if err != nil {
		t.Fatal(err)
	}
	// Advisory locks need >1 connection available (each Lock pins one).
	db.SetMaxOpenConns(4)
	t.Cleanup(func() {
		db.Close()
		_ = exec.Command("dropdb", "--if-exists", dbName).Run()
	})
	return db
}

func TestAdvisoryLockerSerializesAcrossInstances(t *testing.T) {
	db := openLockDB(t, "calserver_advlock")
	nodeA := &caldav.AdvisoryLocker{DB: db}
	nodeB := &caldav.AdvisoryLocker{DB: db} // a "second node" — independent in-process locks

	const path = "calendars/alice/home/e1.ics"
	unlockA := nodeA.Lock(t.Context(), path)

	acquired := make(chan struct{})
	go func() {
		unlockB := nodeB.Lock(t.Context(), path)
		close(acquired)
		unlockB()
	}()

	// B must block while A holds the lock.
	select {
	case <-acquired:
		t.Fatal("node B acquired the write-back lock while node A held it")
	case <-time.After(300 * time.Millisecond):
	}

	unlockA()
	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		t.Fatal("node B never acquired the lock after node A released it")
	}

	// A different object is not blocked by an unrelated held lock.
	other := nodeA.Lock(t.Context(), "calendars/alice/home/e2.ics")
	done := make(chan struct{})
	go func() { u := nodeB.Lock(t.Context(), "calendars/alice/home/e3.ics"); close(done); u() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated paths should not serialize")
	}
	other()
}

// A waiter gives up on the advisory lock after Wait and proceeds under its
// node's own lock, and its abandoned wait leaves no lock behind on a pooled
// connection.
func TestAdvisoryLockerWaitIsBounded(t *testing.T) {
	db := openLockDB(t, "calserver_b_advlock_wait")
	nodeA := &caldav.AdvisoryLocker{DB: db}
	nodeB := &caldav.AdvisoryLocker{DB: db, Wait: 200 * time.Millisecond}

	const path = "calendars/alice/home/e1.ics"
	unlockA := nodeA.Lock(t.Context(), path)
	acquired := make(chan func())
	go func() { acquired <- nodeB.Lock(t.Context(), path) }()
	select {
	case unlockB := <-acquired:
		unlockB()
	case <-time.After(5 * time.Second):
		t.Fatal("node B waited unboundedly for the advisory lock")
	}
	unlockA()

	deadline := time.Now().Add(3 * time.Second)
	for {
		var held int
		if err := db.QueryRow(`SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND granted
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d advisory locks still held after both nodes unlocked", held)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
