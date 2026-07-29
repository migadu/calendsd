package storage_test

// AdminQueries integration test against a scratch Postgres with the sabre
// schema (ADMIN-PLAN.md §6): seeds one of each thing the admin panel
// inspects — a shared calendar, a per-user property overlay, a stale
// change-log row — and asserts the aggregates. Skips when the dev Postgres
// isn't reachable, like the concurrency test.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/storage"
)

func TestAdminQueries(t *testing.T) {
	root := repoRoot(t)
	if reason := pgOnlyPrereqsMissing(); reason != "" {
		t.Skipf("skipping admin queries test: %s", reason)
	}

	const dbName = "calserver_adminqueries"
	run(t, root, "dropdb", "--if-exists", dbName)
	run(t, root, "createdb", dbName)
	sqlFiles, _ := filepath.Glob(filepath.Join(root, "dav/examples/sql/pgsql.*.sql"))
	if len(sqlFiles) == 0 {
		t.Fatal("no schema files found")
	}
	for _, f := range sqlFiles {
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-f", f)
	}
	// The pgsql.principals.sql seed ships principals/admin (+ proxy
	// sub-principals) and a users row for admin. Add alice (a shared
	// calendar owner with an overlay), bob (shared instance, no users
	// row), and an orphaned users row.
	run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-c", `
		INSERT INTO principals (uri, email, displayname) VALUES
			('principals/alice', 'alice@example.org', 'Alice'),
			('principals/bob', 'bob@example.org', 'Bob');
		INSERT INTO users (username, digesta1) VALUES
			('alice', '11111111111111111111111111111111'),
			('orphan', 'not-a-hash');
		INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT');
		INSERT INTO calendarinstances (calendarid, principaluri, access, uri, displayname, calendarcolor)
			VALUES (currval('calendars_id_seq'), 'principals/alice', 1, 'default', 'Alice cal', '#ff0000');
		INSERT INTO calendarinstances (calendarid, principaluri, access, uri, share_href)
			VALUES (currval('calendars_id_seq'), 'principals/bob', 2, 'alice-shared', 'mailto:bob@example.org');
		INSERT INTO propertystorage (path, name, valuetype, value) VALUES
			('calendars/alice/default', '{DAV:}note', 1, 'a note'),
			(E'calendars/alice/default/\n' || 'principals/bob', '{http://apple.com/ns/ical/}calendar-color', 1, '#00ff00'),
			('calendars/alice/default', '{http://sabre.io}php', 3, 'O:8:"stdClass"');
		INSERT INTO addressbooks (principaluri, displayname, uri, synctoken)
			VALUES ('principals/alice', 'Contacts', 'contacts', 1);
	`)

	db, err := storage.Open(fmt.Sprintf("postgres:///%s?host=%s", dbName, os.Getenv("PGHOST")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Write one object through the production backend so the change log
	// and denormalized columns are populated the real way.
	calendars := &storage.CalDAV{DB: db}
	var calID string
	if err := db.QueryRow(`SELECT id FROM calendars ORDER BY id DESC LIMIT 1`).Scan(&calID); err != nil {
		t.Fatal(err)
	}
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:evt-1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260301T100000Z\r\nDTEND:20260301T110000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	id := caldav.CalendarID{Calendar: calID}
	if _, err := calendars.CreateCalendarObject(id, "evt.ics", []byte(ics)); err != nil {
		t.Fatal(err)
	}

	q := &storage.AdminQueries{DB: db}

	counts, err := q.Counts()
	if err != nil {
		t.Fatal(err)
	}
	// 3 seed principals (admin + proxies) + alice + bob.
	if counts.Principals != 5 || counts.Users != 3 || counts.CalendarInstances != 2 ||
		counts.CalendarObjects != 1 || counts.AddressBooks != 1 || counts.PropertyRows != 3 {
		t.Fatalf("counts = %+v", counts)
	}

	diags, err := q.AuthDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	if len(diags.PrincipalsWithoutUser) != 1 || diags.PrincipalsWithoutUser[0] != "principals/bob" {
		t.Fatalf("principals without user = %v, want [principals/bob]", diags.PrincipalsWithoutUser)
	}
	if len(diags.UsersWithoutPrincipal) != 1 || diags.UsersWithoutPrincipal[0] != "orphan" {
		t.Fatalf("users without principal = %v, want [orphan]", diags.UsersWithoutPrincipal)
	}
	if len(diags.MalformedDigestA1) != 1 || diags.MalformedDigestA1[0] != "orphan" {
		t.Fatalf("malformed digests = %v, want [orphan]", diags.MalformedDigestA1)
	}

	user, err := q.UserForPrincipal("principals/alice")
	if err != nil {
		t.Fatal(err)
	}
	if user == nil || user.Username != "alice" {
		t.Fatalf("user for alice = %+v", user)
	}
	if user, err := q.UserForPrincipal("principals/bob"); err != nil || user != nil {
		t.Fatalf("user for bob = %+v err=%v, want nil", user, err)
	}

	gotID, err := q.LookupCalendar("principals/alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	if gotID != calID {
		t.Fatalf("LookupCalendar = %q, want %q", gotID, calID)
	}

	instances, err := q.CalendarInstances(calID)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 || instances[0].Access != 1 || instances[1].Access != 2 {
		t.Fatalf("instances = %+v (owner must sort first)", instances)
	}
	if instances[1].ShareHref != "mailto:bob@example.org" {
		t.Fatalf("share instance = %+v", instances[1])
	}

	meta, err := q.CalendarMeta(calID)
	if err != nil {
		t.Fatal(err)
	}
	// CreateCalendarObject bumped the seeded synctoken 1 → 2.
	if meta.SyncToken != 2 || meta.ObjectCount != 1 || meta.TotalSize != int64(len(ics)) {
		t.Fatalf("meta = %+v", meta)
	}

	props, err := q.PropertyRows("calendars/alice/default")
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 3 {
		t.Fatalf("prop rows = %+v, want 3 (global, overlay, php)", props)
	}
	var overlays, sabreOnly int
	for _, p := range props {
		if p.OverlayPrincipal == "principals/bob" {
			overlays++
		}
		if p.SabreOnly {
			sabreOnly++
		}
	}
	if overlays != 1 || sabreOnly != 1 {
		t.Fatalf("prop layers: overlays=%d sabreOnly=%d in %+v", overlays, sabreOnly, props)
	}

	changes, err := q.RecentChanges(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].PrincipalURI != "principals/alice" ||
		changes[0].ObjectURI != "evt.ics" || changes[0].Operation != 1 ||
		changes[0].LastModified == nil {
		t.Fatalf("changes = %+v", changes)
	}

	objects, truncated, err := q.Objects(storage.AdminObjectQuery{CalendarID: calID, Match: "standup", InData: true})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(objects) != 1 || objects[0].URI != "evt.ics" || objects[0].UID != "evt-1" {
		t.Fatalf("objects = %+v truncated=%t", objects, truncated)
	}
	if objects[0].FirstOccurrence == nil || objects[0].LastOccurrence == nil {
		t.Fatalf("denormalized occurrences missing: %+v", objects[0])
	}
	// uri/uid-only search must not match summary text.
	objects, _, err = q.Objects(storage.AdminObjectQuery{CalendarID: calID, Match: "standup"})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("uri/uid search matched calendardata: %+v", objects)
	}

	detail, err := q.Object(calID, "evt.ics")
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil || string(detail.Data) != ics || detail.Row.ETag == "" {
		t.Fatalf("object detail = %+v", detail)
	}

	bookID, err := q.LookupAddressBook("principals/alice", "contacts")
	if err != nil {
		t.Fatal(err)
	}
	book, err := q.AddressBook(bookID)
	if err != nil {
		t.Fatal(err)
	}
	if book == nil || book.DisplayName != "Contacts" || book.CardCount != 0 {
		t.Fatalf("address book = %+v", book)
	}

	log, err := q.ChangeLog("calendar", calID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].ObjectURI != "evt.ics" || log[0].Operation != 1 {
		t.Fatalf("change log = %+v", log)
	}

	// Phase B: seed every broken invariant the doctor must catch, plus a
	// pending share invite.
	run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-c", `
		UPDATE calendarinstances SET share_invitestatus = 1 WHERE principaluri = 'principals/bob';
		INSERT INTO calendarinstances (calendarid, principaluri, access, uri)
			VALUES (9999, 'principals/alice', 1, 'ghost-cal');
		INSERT INTO groupmembers (principal_id, member_id) VALUES (99991, 99992);
		INSERT INTO propertystorage (path, name, valuetype, value)
			VALUES ('calendars/nobody/gone', '{DAV:}note', 1, 'orphan');
		INSERT INTO calendarobjects (calendardata, uri, calendarid, lastmodified, etag, size, componenttype)
			VALUES ('NOT AN ICAL', 'broken.ics', `+calID+`, 0, 'x', 11, 'VEVENT');
	`)
	if _, err := db.Exec(
		`UPDATE calendarobjects SET size = size + 7 WHERE uri = 'evt.ics'`); err != nil {
		t.Fatal(err)
	}

	invites, err := q.PendingShareInvites("principals/bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(invites) != 1 || invites[0].PrincipalURI != "principals/bob" || invites[0].InviteStatus != 1 {
		t.Fatalf("pending invites = %+v", invites)
	}
	if none, err := q.PendingShareInvites("principals/alice"); err != nil || len(none) != 0 {
		t.Fatalf("alice pending invites = %+v err=%v, want none", none, err)
	}

	report, err := q.Doctor(0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, f := range report.Findings {
		got[f.Check]++
	}
	for check, want := range map[string]int{
		"orphaned-instance":          1, // ghost-cal → calendars id 9999
		"dangling-groupmember":       1,
		"orphaned-property":          1, // calendars/nobody/gone
		"unparseable-calendardata":   1, // broken.ics
		"stale-denormalized-columns": 1, // evt.ics size bumped
		"principal-without-user":     1, // bob (from the earlier seed)
		"user-without-principal":     1, // orphan
		"malformed-digesta1":         1, // orphan
	} {
		if got[check] != want {
			t.Errorf("doctor: %s findings = %d, want %d (all: %+v)", check, got[check], want, report.Findings)
		}
	}
	if report.ObjectsScanned != 2 || report.Truncated {
		t.Fatalf("doctor scan: objects=%d truncated=%t", report.ObjectsScanned, report.Truncated)
	}
	// The valid per-user overlay and collection props must NOT be flagged.
	if got["orphaned-property"] != 1 {
		t.Fatalf("valid propertystorage rows were flagged: %+v", report.Findings)
	}

	// Phase C: the AdminWrites carve-out.
	writes := &storage.AdminWrites{DB: db}
	carolDigest := auth.DigestA1("carol", "SabreDAV", "pw1")
	if err := writes.CreateUser("carol", carolDigest, "carol@example.org", "Carol"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := writes.CreateUser("carol", carolDigest, "", ""); err == nil {
		t.Fatal("duplicate CreateUser succeeded; want error")
	}
	carol, err := q.UserForPrincipal("principals/carol")
	if err != nil || carol == nil || carol.DigestA1 != carolDigest {
		t.Fatalf("carol user = %+v err=%v", carol, err)
	}
	var principalRows int
	if err := db.QueryRow(
		`SELECT count(*) FROM principals WHERE uri = 'principals/carol' OR uri LIKE 'principals/carol/%'`).Scan(&principalRows); err != nil {
		t.Fatal(err)
	}
	if principalRows != 3 {
		t.Fatalf("carol principal rows = %d, want 3 (principal + read/write proxies)", principalRows)
	}
	newDigest := auth.DigestA1("carol", "SabreDAV", "pw2")
	if err := writes.SetPassword("carol", newDigest); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if carol, _ := q.UserForPrincipal("principals/carol"); carol.DigestA1 != newDigest {
		t.Fatalf("password not updated: %+v", carol)
	}
	if err := writes.SetPassword("no-such-user", newDigest); err == nil {
		t.Fatal("SetPassword for a missing user succeeded; want error")
	}

	// UpdatePrincipal: set both fields, then clear one; nil leaves alone.
	newEmail, newName, empty := "carol2@example.org", "Carol II", ""
	if err := writes.UpdatePrincipal("principals/carol", &newEmail, &newName); err != nil {
		t.Fatalf("UpdatePrincipal: %v", err)
	}
	var email, displayname string
	if err := db.QueryRow(`SELECT coalesce(email, ''), coalesce(displayname, '')
		FROM principals WHERE uri = 'principals/carol'`).Scan(&email, &displayname); err != nil {
		t.Fatal(err)
	}
	if email != newEmail || displayname != newName {
		t.Fatalf("after update: email=%q displayname=%q", email, displayname)
	}
	if err := writes.UpdatePrincipal("principals/carol", &empty, nil); err != nil {
		t.Fatalf("UpdatePrincipal clear: %v", err)
	}
	if err := db.QueryRow(`SELECT coalesce(email, ''), coalesce(displayname, '')
		FROM principals WHERE uri = 'principals/carol'`).Scan(&email, &displayname); err != nil {
		t.Fatal(err)
	}
	if email != "" || displayname != newName {
		t.Fatalf("after clear: email=%q displayname=%q, want cleared email only", email, displayname)
	}
	if err := writes.UpdatePrincipal("principals/carol", nil, nil); err == nil {
		t.Fatal("UpdatePrincipal with nothing to change succeeded; want error")
	}
	if err := writes.UpdatePrincipal("principals/no-such", &newEmail, nil); err == nil {
		t.Fatal("UpdatePrincipal for a missing principal succeeded; want error")
	}

	// DeleteUser: refuses while collections exist, otherwise removes the
	// users row, principal triple, and group-membership rows.
	if err := writes.DeleteUser("alice"); err == nil {
		t.Fatal("DeleteUser for alice (owns a calendar and address book) succeeded; want refusal")
	}
	if _, err := db.Exec(`INSERT INTO groupmembers (principal_id, member_id)
		SELECT g.id, m.id FROM principals g, principals m
		 WHERE g.uri = 'principals/admin/calendar-proxy-read' AND m.uri = 'principals/carol'`); err != nil {
		t.Fatal(err)
	}
	var carolID int64
	if err := db.QueryRow(`SELECT id FROM principals WHERE uri = 'principals/carol'`).Scan(&carolID); err != nil {
		t.Fatal(err)
	}
	if err := writes.DeleteUser("carol"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	var leftover int
	if err := db.QueryRow(`SELECT
		(SELECT count(*) FROM users WHERE username = 'carol') +
		(SELECT count(*) FROM principals WHERE uri = 'principals/carol' OR uri LIKE 'principals/carol/%') +
		(SELECT count(*) FROM groupmembers WHERE member_id = $1 OR principal_id = $1)`,
		carolID).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatalf("carol left %d row(s) behind", leftover)
	}
	if err := writes.DeleteUser("carol"); err == nil {
		t.Fatal("DeleteUser for a deleted user succeeded; want error")
	}

	// Deleting the doctor's orphans must clear exactly those findings.
	for _, check := range []string{"orphaned-instance", "dangling-groupmember", "orphaned-property"} {
		n, err := writes.DeleteOrphans(check)
		if err != nil {
			t.Fatalf("DeleteOrphans(%s): %v", check, err)
		}
		if n != 1 {
			t.Errorf("DeleteOrphans(%s) = %d rows, want 1", check, n)
		}
	}
	if _, err := writes.DeleteOrphans("stale-denormalized-columns"); err == nil {
		t.Fatal("non-deletable check accepted; want error")
	}
	report, err = q.Doctor(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Findings {
		switch f.Check {
		case "orphaned-instance", "dangling-groupmember", "orphaned-property":
			t.Errorf("finding survived its DeleteOrphans: %+v", f)
		}
	}

	// Truncation honesty (evt.ics + broken.ics exist at this point): a hit
	// limit only counts as truncation when further MATCHES exist, not
	// merely further rows.
	objs, trunc, err := q.Objects(storage.AdminObjectQuery{CalendarID: calID, Limit: 1})
	if err != nil || len(objs) != 1 || !trunc {
		t.Fatalf("unfiltered limit=1: %d rows trunc=%t err=%v, want 1/true", len(objs), trunc, err)
	}
	objs, trunc, err = q.Objects(storage.AdminObjectQuery{CalendarID: calID, Match: "evt", Limit: 1})
	if err != nil || len(objs) != 1 || trunc {
		t.Fatalf("uri match limit=1: %d rows trunc=%t err=%v, want 1/false (only one match exists)", len(objs), trunc, err)
	}
	objs, trunc, err = q.Objects(storage.AdminObjectQuery{CalendarID: calID, Match: "standup", InData: true, Limit: 1})
	if err != nil || len(objs) != 1 || trunc {
		t.Fatalf("in-data match limit=1: %d rows trunc=%t err=%v, want 1/false", len(objs), trunc, err)
	}

	// DeleteUserCascade: alice owns a calendar (shared to bob) and an address
	// book with objects, so plain DeleteUser must still refuse. Cascade drops
	// the owned collections and their data — including bob's sharee instance
	// of alice's calendar — then removes the principal triple and users row.
	// Run last: it destroys the fixture alice everything above relies on.
	if err := writes.DeleteUser("alice"); err == nil {
		t.Fatal("DeleteUser for alice (owns collections) succeeded; cascade must be explicit")
	}
	if err := writes.DeleteUserCascade("alice"); err != nil {
		t.Fatalf("DeleteUserCascade: %v", err)
	}
	var aliceLeft int
	if err := db.QueryRow(`SELECT
		(SELECT count(*) FROM users WHERE username = 'alice') +
		(SELECT count(*) FROM principals WHERE uri = 'principals/alice' OR uri LIKE 'principals/alice/%') +
		(SELECT count(*) FROM calendarinstances WHERE principaluri = 'principals/alice') +
		(SELECT count(*) FROM calendars WHERE id = `+calID+`) +
		(SELECT count(*) FROM calendarobjects WHERE calendarid = `+calID+`) +
		(SELECT count(*) FROM calendarchanges WHERE calendarid = `+calID+`) +
		(SELECT count(*) FROM addressbooks WHERE principaluri = 'principals/alice')`).Scan(&aliceLeft); err != nil {
		t.Fatal(err)
	}
	if aliceLeft != 0 {
		t.Fatalf("cascade left %d alice-owned row(s) behind", aliceLeft)
	}
	// Bob's only instance was a share of alice's now-deleted calendar, so it
	// must be gone; bob's own principal/users rows must survive untouched.
	var bobInstances, bobPrincipal int
	if err := db.QueryRow(`SELECT
		(SELECT count(*) FROM calendarinstances WHERE principaluri = 'principals/bob'),
		(SELECT count(*) FROM principals WHERE uri = 'principals/bob')`).Scan(&bobInstances, &bobPrincipal); err != nil {
		t.Fatal(err)
	}
	if bobInstances != 0 {
		t.Fatalf("bob kept %d instance(s) of alice's deleted calendar", bobInstances)
	}
	if bobPrincipal != 1 {
		t.Fatalf("cascade removed bob's principal (%d); it must only drop alice's own rows", bobPrincipal)
	}
	// A second cascade of the now-absent alice must error, like DeleteUser.
	if err := writes.DeleteUserCascade("alice"); err == nil {
		t.Fatal("DeleteUserCascade for a deleted user succeeded; want error")
	}
	if err := writes.DeleteUserCascade("bad/name"); err == nil {
		t.Fatal("DeleteUserCascade accepted an invalid username; want error")
	}
}
