package storage_test

// AdminWrites regression tests against a scratch Postgres with the sabre
// schema plus the calendsd extension tables. Postgres-only; skips when the
// dev Postgres isn't reachable.

import (
	"testing"
	"time"

	"calendsd/internal/caldav"
	"calendsd/internal/storage"
)

// TestPurgeExpiredKeepsTombstoneOnFailedCascade covers PurgeExpired claiming
// (deleting) the tombstone in its own transaction before the cascade: a
// cascade that then failed left the account intact but with no tombstone, so
// the auth gate let it log in again and no later sweep would retry it. The
// claim must roll back with the cascade.
func TestPurgeExpiredKeepsTombstoneOnFailedCascade(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_d_purge")
	writes := &storage.AdminWrites{DB: db}
	if err := writes.CreateUser(t.Context(), "eve", storage.BlockedDigestA1(), "eve@example.org", "Eve"); err != nil {
		t.Fatal(err)
	}
	if err := writes.ScheduleDelete(t.Context(), "eve", 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE deleted_principals SET purge_after = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	// Make the cascade fail partway: any delete from contact_frecency raises.
	if _, err := db.Exec(`
CREATE FUNCTION fail_purge() RETURNS trigger LANGUAGE plpgsql AS
  $$ BEGIN RAISE EXCEPTION 'injected purge failure'; END $$;
CREATE TRIGGER fail_purge BEFORE DELETE ON contact_frecency
  FOR EACH STATEMENT EXECUTE FUNCTION fail_purge();`); err != nil {
		t.Fatal(err)
	}
	if _, err := writes.PurgeExpired(t.Context()); err == nil {
		t.Fatal("PurgeExpired succeeded despite the injected cascade failure")
	}
	users := &storage.Users{DB: db}
	if del, err := users.IsDeleted(t.Context(), "principals/eve"); err != nil || !del {
		t.Fatalf("IsDeleted(eve) after a failed purge = %v, %v; want the tombstone kept", del, err)
	}

	// With the failure gone the next sweep purges the account.
	if _, err := db.Exec(`DROP TRIGGER fail_purge ON contact_frecency`); err != nil {
		t.Fatal(err)
	}
	purged, err := writes.PurgeExpired(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || purged[0].Username != "eve" {
		t.Fatalf("PurgeExpired = %+v, want eve", purged)
	}
	var left int
	if err := db.QueryRow(`SELECT
		(SELECT count(*) FROM principals WHERE uri LIKE 'principals/eve%') +
		(SELECT count(*) FROM deleted_principals)`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d principal/tombstone row(s) left after the purge", left)
	}
}

// TestHashA1TellsOutageFromUnknownUser covers Users.HashA1 folding every
// error into ok=false, so a database outage read as a wrong password.
func TestHashA1TellsOutageFromUnknownUser(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_d_hasha1")
	if err := (&storage.AdminWrites{DB: db}).CreateUser(t.Context(), "carol", "0123456789abcdef0123456789abcdef", "", ""); err != nil {
		t.Fatal(err)
	}
	users := &storage.Users{DB: db}
	if hash, err := users.HashA1(t.Context(), "SabreDAV", "carol"); err != nil || hash != "0123456789abcdef0123456789abcdef" {
		t.Errorf("HashA1(carol) = %q, %v", hash, err)
	}
	if hash, err := users.HashA1(t.Context(), "SabreDAV", "nobody"); err != nil || hash != "" {
		t.Errorf("HashA1(nobody) = %q, %v; want \"\", nil", hash, err)
	}
	db.Close()
	if _, err := users.HashA1(t.Context(), "SabreDAV", "carol"); err == nil {
		t.Error("HashA1 on a closed database returned no error")
	}
}

// TestDeleteUserCascadeDropsNotifications covers the cascade leaving the
// principal's share-invite notifications behind: nothing else removes them,
// and a later account with the same name inherited the invites.
func TestDeleteUserCascadeDropsNotifications(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_d_cascade_notif")
	writes := &storage.AdminWrites{DB: db}
	if err := writes.CreateUser(t.Context(), "bob", storage.BlockedDigestA1(), "bob@example.org", "Bob"); err != nil {
		t.Fatal(err)
	}
	cal := &storage.CalDAV{DB: db}
	if err := cal.AddNotification(t.Context(), "principals/bob", caldav.InviteNotification{
		ID: "invite-1", DTStamp: time.Now(), HostURL: "/calendars/alice/default/",
		Organizer: "mailto:alice@example.org", Access: caldav.AccessRead, InviteStatus: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writes.DeleteUserCascade(t.Context(), "bob"); err != nil {
		t.Fatal(err)
	}
	// Recreate the account: it must start with an empty inbox.
	if err := writes.CreateUser(t.Context(), "bob", storage.BlockedDigestA1(), "bob@example.org", "Bob"); err != nil {
		t.Fatal(err)
	}
	got, err := cal.NotificationsForPrincipal(t.Context(), "principals/bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("recreated principal inherited %d notification(s): %+v", len(got), got)
	}
}
