package storage_test

// calendar_availability against a scratch Postgres: the one-time move of
// inbox dead properties when the extension migration is applied, the backend
// round-trip, the user-delete cleanup and the doctor's orphan check
// (VAVAILABILITY-PLAN.md §3). Skips when the dev Postgres isn't reachable.

import (
	"strings"
	"testing"

	"calendsd/internal/acl"
	"calendsd/internal/storage"
	"calendsd/internal/storage/storagetest"
	"calendsd/scripts/schema"
)

func TestCalendarAvailability(t *testing.T) {
	const dbName = "calserver_availability"
	// The sabre tables only, as before the extension schema existed.
	db := storagetest.CreateDB(t, dbName)
	mustExec(t, db, schema.Migrations()[0].SQL)
	// Dead properties as a client wrote them before the table existed:
	// alice under both names (the CalendarServer one must win), bob under
	// the RFC name only, a deleted user's row, and an unrelated inbox
	// property that must stay.
	mustExec(t, db, `
		INSERT INTO principals (uri, email, displayname) VALUES
			('principals/alice', 'alice@example.org', 'Alice'),
			('principals/bob', 'bob@example.org', 'Bob'),
			('principals/carol', 'carol@example.org', 'Carol'),
			('principals/dave', 'dave@example.org', 'Dave');
		INSERT INTO propertystorage (path, name, valuetype, value) VALUES
			('calendars/alice/inbox', '{http://calendarserver.org/ns/}calendar-availability', 1, E'BEGIN:VCALENDAR\nX-WHO:alice-cs\nEND:VCALENDAR\n'),
			('calendars/alice/inbox', '{urn:ietf:params:xml:ns:caldav}calendar-availability', 1, E'BEGIN:VCALENDAR\nX-WHO:alice-rfc\nEND:VCALENDAR\n'),
			('calendars/bob/inbox', '{urn:ietf:params:xml:ns:caldav}calendar-availability', 1, E'BEGIN:VCALENDAR\r\nX-WHO:bob\r\nEND:VCALENDAR\r\n'),
			('calendars/ghost/inbox', '{http://calendarserver.org/ns/}calendar-availability', 1, 'ghost'),
			('calendars/carol/inbox', '{http://calendarserver.org/ns/}calendar-availability', 1, decode('fffe', 'hex')),
			('calendars/dave/inbox', '{http://calendarserver.org/ns/}calendar-availability', 1, NULL),
			('calendars/alice/inbox', '{DAV:}note', 1, 'keep me');
	`)
	if _, err := schema.Apply(t.Context(), db, 0); err != nil {
		t.Fatal(err)
	}
	backend := &storage.CalDAV{DB: db}

	get := func(principal string) string {
		t.Helper()
		data, err := backend.Availability(t.Context(), principal)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := get("principals/alice"); got != "BEGIN:VCALENDAR\r\nX-WHO:alice-cs\r\nEND:VCALENDAR\r\n" {
		t.Errorf("alice imported %q, want the CalendarServer value with CRLF", got)
	}
	if got := get("principals/bob"); got != "BEGIN:VCALENDAR\r\nX-WHO:bob\r\nEND:VCALENDAR\r\n" {
		t.Errorf("bob imported %q (CRLF must not double)", got)
	}
	countDead := func() (availability, other int) {
		t.Helper()
		if err := db.QueryRow(`SELECT
			count(*) FILTER (WHERE name LIKE '%calendar-availability'),
			count(*) FILTER (WHERE name NOT LIKE '%calendar-availability')
			FROM propertystorage`).Scan(&availability, &other); err != nil {
			t.Fatal(err)
		}
		return
	}
	// Left behind: ghost's row (no principal to move it to), carol's
	// non-UTF-8 bytes and dave's NULL, none of which may stop the schema
	// run. The unrelated inbox property is untouched.
	if a, o := countDead(); a != 3 || o != 1 {
		t.Errorf("after import: %d availability and %d other dead rows, want 3 and 1", a, o)
	}
	for _, p := range []string{"principals/carol", "principals/dave"} {
		if got := get(p); got != "" {
			t.Errorf("%s imported %q from an unusable value", p, got)
		}
	}

	// Running the extension migration again (as the runner does once on a
	// database from before it) moves nothing and keeps newer values.
	if err := backend.SetAvailability(t.Context(), "principals/alice", []byte("newer")); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, schema.Migrations()[1].SQL)
	if got := get("principals/alice"); got != "newer" {
		t.Errorf("re-run replaced alice's value with %q", got)
	}

	// Backend round-trip.
	if got := get("principals/nobody"); got != "" {
		t.Errorf("unset availability = %q, want none", got)
	}
	if err := backend.DeleteAvailability(t.Context(), "principals/bob"); err != nil {
		t.Fatal(err)
	}
	if got := get("principals/bob"); got != "" {
		t.Errorf("deleted availability still reads %q", got)
	}

	// Doctor: a row for a missing principal is flagged and deletable; the
	// live ones are not.
	if err := backend.SetAvailability(t.Context(), "principals/nobody", []byte("x")); err != nil {
		t.Fatal(err)
	}
	report, err := (&storage.AdminQueries{DB: db}).Doctor(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var flagged []string
	for _, f := range report.Findings {
		if f.Check == "orphaned-availability" {
			flagged = append(flagged, f.Subject)
		}
	}
	if len(flagged) != 1 || flagged[0] != "availability of principals/nobody" {
		t.Errorf("orphaned-availability findings = %v, want just principals/nobody", flagged)
	}
	writes := &storage.AdminWrites{DB: db}
	if n, err := writes.DeleteOrphans(t.Context(), "orphaned-availability"); err != nil || n != 1 {
		t.Errorf("DeleteOrphans = %d, %v; want 1", n, err)
	}
	if got := get("principals/alice"); got != "newer" {
		t.Error("DeleteOrphans removed a live user's availability")
	}

	// Free-busy visibility round-trip (VAVAILABILITY-PLAN.md §10).
	vis := func(principal string) acl.FreeBusyVisibility {
		t.Helper()
		v, err := backend.FreeBusyVisibility(t.Context(), principal)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := vis("principals/alice"); v.Hidden || len(v.Allowed) != 0 {
		t.Errorf("unset visibility = %+v, want the default", v)
	}
	if err := backend.SetFreeBusyVisibility(t.Context(), "principals/alice",
		acl.FreeBusyVisibility{Hidden: true, Allowed: []string{"principals/bob", "principals/carol"}}); err != nil {
		t.Fatal(err)
	}
	if v := vis("principals/alice"); !v.Hidden || strings.Join(v.Allowed, ",") != "principals/bob,principals/carol" {
		t.Errorf("alice visibility = %+v", v)
	}
	if err := backend.SetFreeBusyVisibility(t.Context(), "principals/alice", acl.FreeBusyVisibility{}); err != nil {
		t.Fatal(err)
	}
	if v := vis("principals/alice"); v.Hidden || len(v.Allowed) != 0 {
		t.Errorf("replaced visibility = %+v, want domain with nobody extra", v)
	}
	if err := backend.SetFreeBusyVisibility(t.Context(), "principals/alice",
		acl.FreeBusyVisibility{Allowed: []string{"principals/carol"}}); err != nil {
		t.Fatal(err)
	}
	if err := backend.SetFreeBusyVisibility(t.Context(), "principals/bob",
		acl.FreeBusyVisibility{Hidden: true, Allowed: []string{"principals/alice", "mailto:alice@example.org", "principals/carol"}}); err != nil {
		t.Fatal(err)
	}
	if err := backend.SetFreeBusyVisibility(t.Context(), "principals/nobody", acl.FreeBusyVisibility{Hidden: true}); err != nil {
		t.Fatal(err)
	}
	report, err = (&storage.AdminQueries{DB: db}).Doctor(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	flagged = nil
	for _, f := range report.Findings {
		if f.Check == "orphaned-visibility" {
			flagged = append(flagged, f.Subject)
		}
	}
	if len(flagged) != 1 || flagged[0] != "free-busy visibility of principals/nobody" {
		t.Errorf("orphaned-visibility findings = %v", flagged)
	}
	if n, err := writes.DeleteOrphans(t.Context(), "orphaned-visibility"); err != nil || n != 1 {
		t.Errorf("DeleteOrphans(orphaned-visibility) = %d, %v; want 1", n, err)
	}

	// Deleting the user deletes their working hours, their visibility, and
	// their place on everyone else's allowed list.
	if err := writes.DeleteUserCascade(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if got := get("principals/alice"); got != "" {
		t.Errorf("DeleteUserCascade left availability %q", got)
	}
	if v := vis("principals/alice"); v.Hidden || len(v.Allowed) != 0 {
		t.Errorf("DeleteUserCascade left alice's visibility %+v", v)
	}
	if v := vis("principals/bob"); !v.Hidden || strings.Join(v.Allowed, ",") != "principals/carol" {
		t.Errorf("bob's visibility after alice's deletion = %+v, want only carol", v)
	}
}
