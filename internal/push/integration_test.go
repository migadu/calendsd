package push

// End-to-end tests against a scratch Postgres loaded with the sabre + calserver
// schema. Skips automatically when the dev Postgres isn't reachable, like the
// storage package's Postgres tests. Internal (package push) so it can drive the
// unexported dispatch path directly.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"calserver/internal/storage"
)

func testDSN(dbName string) string {
	return fmt.Sprintf("postgres:///%s?host=%s", dbName, os.Getenv("PGHOST"))
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// openScratchDB builds a fresh database from the sabre schema plus the
// calserver extension schema (which defines push_subscriptions).
func openScratchDB(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	if os.Getenv("PGHOST") == "" {
		t.Skip("skipping: PGHOST not set (dev Postgres not running)")
	}
	if err := exec.Command("pg_isready", "-h", os.Getenv("PGHOST")).Run(); err != nil {
		t.Skipf("skipping: dev Postgres not reachable: %v", err)
	}
	root := repoRoot()
	psql := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	psql("dropdb", "--if-exists", dbName)
	psql("createdb", dbName)
	sqlFiles, _ := filepath.Glob(filepath.Join(root, "dav/examples/sql/pgsql.*.sql"))
	if len(sqlFiles) == 0 {
		t.Fatal("no sabre schema files found")
	}
	for _, f := range sqlFiles {
		psql("psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-f", f)
	}
	psql("psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-f",
		filepath.Join(root, "scripts/schema/pgsql.calserver.sql"))

	db, err := storage.Open(testDSN(dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestChangeFiresNotify verifies a committed calendar-object write emits the
// dav_push NOTIFY the worker listens for — the whole trigger mechanism.
func TestChangeFiresNotify(t *testing.T) {
	db := openScratchDB(t, "calserver_push_notify")
	backend := &storage.CalDAV{DB: db}
	id, err := backend.CreateCalendar("principals/alice", "home", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, testDSN("calserver_push_notify"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN dav_push"); err != nil {
		t.Fatal(err)
	}

	if _, err := backend.CreateCalendarObject(id, "e.ics", []byte(sampleICS)); err != nil {
		t.Fatal(err)
	}
	n, err := conn.WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("no notification: %v", err)
	}
	if want := "calendar:" + id.Calendar; n.Payload != want {
		t.Errorf("payload = %q, want %q", n.Payload, want)
	}
}

// TestStoreRoundTrip covers upsert idempotency, lookup, token seeding, and
// principal-scoped delete.
func TestStoreRoundTrip(t *testing.T) {
	db := openScratchDB(t, "calserver_push_store")
	backend := &storage.CalDAV{DB: db}
	id, err := backend.CreateCalendar("principals/alice", "home", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{DB: db}

	cur, ok, err := store.CurrentToken("calendar", id.Calendar)
	if err != nil || !ok {
		t.Fatalf("CurrentToken: %v ok=%v", err, ok)
	}
	sub := Subscription{
		Token: "tok1", Kind: "calendar", CollectionID: id.Calendar,
		CollectionPath: "calendars/alice/home", PrincipalURI: "principals/alice",
		PushResource: "https://push.example/a", P256dh: "PUB", AuthSecret: "AUTH",
	}
	token, err := store.Upsert(sub, cur, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if token != "tok1" {
		t.Errorf("token = %q, want tok1", token)
	}

	// Re-registering the same endpoint keeps the original token (idempotent).
	sub2 := sub
	sub2.Token = "tok2"
	sub2.P256dh = "NEWPUB"
	token2, err := store.Upsert(sub2, cur, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if token2 != "tok1" {
		t.Errorf("re-register token = %q, want the original tok1", token2)
	}
	subs, err := store.ByCollection("calendar", id.Calendar)
	if err != nil || len(subs) != 1 {
		t.Fatalf("ByCollection: %v n=%d (want 1 after idempotent re-register)", err, len(subs))
	}
	if subs[0].P256dh != "NEWPUB" || subs[0].LastSyncToken != cur {
		t.Errorf("re-register did not refresh keys / touched last_synctoken: %+v", subs[0])
	}

	// A different principal cannot delete it; the owner can.
	if ok, _ := store.DeleteByToken("tok1", "principals/bob"); ok {
		t.Error("cross-principal delete succeeded")
	}
	if ok, _ := store.DeleteByToken("tok1", "principals/alice"); !ok {
		t.Error("owner delete failed")
	}
}

// fakeSender records deliveries and returns a fixed status.
type fakeSender struct {
	mu      sync.Mutex
	status  int
	payload []byte
	count   int
}

func (f *fakeSender) Send(_ context.Context, _ Subscription, _ string, payload []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	f.payload = payload
	return f.status, nil
}

// okChecker grants {DAV:}read unconditionally, standing in for the ACL plugin.
type okChecker struct{}

func (okChecker) CheckPrivileges(_ context.Context, _ string, _ ...string) error { return nil }

// TestDispatchDeliversAndDedups drives the worker's core dispatch: a change is
// delivered once, a repeat is a no-op, and a Gone endpoint is pruned.
func TestDispatchDeliversAndDedups(t *testing.T) {
	db := openScratchDB(t, "calserver_push_dispatch")
	backend := &storage.CalDAV{DB: db}
	id, err := backend.CreateCalendar("principals/alice", "home", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CreateCalendarObject(id, "e.ics", []byte(sampleICS)); err != nil {
		t.Fatal(err)
	}
	store := &Store{DB: db}
	cur, _, _ := store.CurrentToken("calendar", id.Calendar)
	// Seed last=0 so the subscription is behind the current token.
	if _, err := store.Upsert(Subscription{
		Token: "t", Kind: "calendar", CollectionID: id.Calendar,
		CollectionPath: "calendars/alice/home", PrincipalURI: "principals/alice",
		PushResource: "https://push.example/a", P256dh: "PUB", AuthSecret: "AUTH",
	}, 0, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	sender := &fakeSender{status: 201}
	w := &Worker{cfg: Config{TopicSecret: "s"}, store: store, sender: sender,
		checker: okChecker{}, log: slog.Default()}
	ctx := context.Background()

	w.dispatch(ctx, "calendar:"+id.Calendar)
	if sender.count != 1 {
		t.Fatalf("expected 1 delivery, got %d", sender.count)
	}
	if !strings.Contains(string(sender.payload), strconv.FormatInt(cur, 10)) {
		t.Errorf("payload missing sync token %d: %s", cur, sender.payload)
	}
	// last_synctoken now caught up → a repeat dispatch delivers nothing.
	w.dispatch(ctx, "calendar:"+id.Calendar)
	if sender.count != 1 {
		t.Errorf("repeat dispatch re-delivered: count=%d", sender.count)
	}

	// A new change plus a Gone endpoint prunes the subscription.
	if _, err := backend.CreateCalendarObject(id, "f.ics", []byte(sampleICS)); err != nil {
		t.Fatal(err)
	}
	sender.status = 410
	w.dispatch(ctx, "calendar:"+id.Calendar)
	left, _ := store.ByCollection("calendar", id.Calendar)
	if len(left) != 0 {
		t.Errorf("Gone endpoint not pruned: %d remain", len(left))
	}
}

const sampleICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:evt-1\r\nDTSTAMP:20240101T000000Z\r\nDTSTART:20240101T120000Z\r\n" +
	"SUMMARY:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
