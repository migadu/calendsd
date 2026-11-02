package storage_test

import (
	"testing"

	"github.com/migadu/calendsd/internal/storage"
	"github.com/migadu/calendsd/internal/storage/storagetest"
)

// Deleting an account also drops its share notifications (received and
// sent) and push subscriptions, and deleting a calendar drops the
// subscriptions on it; neither table has a foreign key (audit 6.5).
func TestCascadeDropsNotificationsAndPushSubscriptions(t *testing.T) {
	db := storagetest.NewDB(t, "calserver_purgerows")
	ctx := t.Context()
	writes := &storage.AdminWrites{DB: db}
	for _, u := range []string{"alice", "bob"} {
		if err := writes.CreateUser(ctx, u, storage.BlockedDigestA1(), u+"@example.org", u); err != nil {
			t.Fatal(err)
		}
	}
	cal := &storage.CalDAV{DB: db}
	id, err := cal.CreateCalendar(ctx, "principals/alice", "home", nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO notifications (principaluri, entryid, dtstamp, hosturl, organizer, access, invitestatus)
	      VALUES ('principals/bob', 'e1', 1, 'calendars/alice/home', 'principals/alice', 2, 1),
	             ('principals/alice', 'e2', 1, 'calendars/bob/x', 'principals/bob', 2, 1)`)
	exec(`INSERT INTO push_subscriptions (token, collection_kind, collection_id, collection_path, principal_uri, push_resource, p256dh, auth_secret, expires)
	      VALUES ('t1', 'calendar', $1, 'calendars/alice/home', 'principals/bob', 'https://push.example/1', 'k', 'a', now() + interval '1 day'),
	             ('t2', 'calendar', $1, 'calendars/alice/home', 'principals/alice', 'https://push.example/2', 'k', 'a', now() + interval '1 day')`, id.Calendar)

	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := writes.DeleteUserCascade(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM notifications`); n != 0 {
		t.Errorf("%d notification rows survived alice's purge (received by or sent from her)", n)
	}
	if n := count(`SELECT count(*) FROM push_subscriptions`); n != 0 {
		t.Errorf("%d push subscriptions survived (bob's on alice's deleted calendar, and alice's own)", n)
	}
}
