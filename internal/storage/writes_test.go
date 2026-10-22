package storage_test

// Write-path regression tests for the CalDAV and CardDAV backends against a
// scratch Postgres with the sabre schema. Postgres-only; skips when the dev
// Postgres isn't reachable.

import (
	"encoding/xml"
	"errors"
	"reflect"
	"sort"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/storage"
)

// TestArrayColumnsRoundTrip pins the TEXT[] / INT[] columns now bound and
// scanned as native pgx arrays (they went through array_to_json and a
// hand-built literal): values with array-literal metacharacters survive, and
// an empty list is stored as '{}', not NULL.
func TestArrayColumnsRoundTrip(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_d_arrays")
	cal := &storage.CalDAV{DB: db}
	awkward := []string{"principals/a,b", `mailto:"q"@x`, "{brace}", "NULL", `back\slash`, ""}
	if err := cal.SetFreeBusyVisibility(t.Context(), "principals/alice", acl.FreeBusyVisibility{Allowed: awkward}); err != nil {
		t.Fatal(err)
	}
	got, err := cal.FreeBusyVisibility(t.Context(), "principals/alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Allowed, awkward) {
		t.Errorf("Allowed round trip = %q, want %q", got.Allowed, awkward)
	}
	if err := cal.SetFreeBusyVisibility(t.Context(), "principals/alice", acl.FreeBusyVisibility{Hidden: true}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRow(`SELECT allowed::text FROM free_busy_visibility WHERE principal = 'principals/alice'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "{}" {
		t.Errorf("empty allowed list stored as %q, want {}", stored)
	}

	// The import ledger's source_users, with and without ids.
	imp := &storage.ContactImport{DB: db}
	for principal, ids := range map[string][]int32{
		"two@example.org":  {7, -9},
		"none@example.org": nil,
	} {
		req := baseRequest(principal, []storage.ImportCard{card("a", "A")})
		req.SourceUsers = ids
		mustImport(t, imp, req)
		var users string
		if err := db.QueryRow(`SELECT source_users::text FROM contact_import WHERE principal = $1`,
			"principals/"+principal).Scan(&users); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"two@example.org": "{7,-9}", "none@example.org": "{}"}[principal]
		if users != want {
			t.Errorf("%s: source_users = %q, want %q", principal, users, want)
		}
	}
}

// TestPropertyMoveRenamesSubtree pins PropertyStorage.Move, now one UPDATE
// rather than a select and a rename per row: the path and its children move
// (multi-byte characters and LIKE metacharacters included), siblings that
// merely share a prefix do not.
func TestPropertyMoveRenamesSubtree(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_d_propmove")
	if _, err := db.Exec(`INSERT INTO propertystorage (path, name, valuetype, value) VALUES
		('calendars/ä_1/cal', '{DAV:}a', 1, 'x'),
		('calendars/ä_1/cal/child.ics', '{DAV:}a', 1, 'x'),
		(E'calendars/ä_1/cal/\nprincipals/bob', '{DAV:}a', 1, 'x'),
		('calendars/ä_1/calendar', '{DAV:}a', 1, 'x'),
		('calendars/äx1/cal', '{DAV:}a', 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	if err := (&storage.PropertyStorage{DB: db}).Move(t.Context(), "calendars/ä_1/cal", "calendars/bö/new"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT path FROM propertystorage ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	want := []string{
		"calendars/bö/new", "calendars/bö/new/\nprincipals/bob", "calendars/bö/new/child.ics",
		"calendars/äx1/cal", "calendars/ä_1/calendar",
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths after Move = %q, want %q", got, want)
	}
}

// TestNullETagReadsAsDataMD5 covers legacy rows with a NULL etag column
// (written by something other than this server or sabre) reaching clients as
// the empty etag `""`. Reads now surface md5 of the data, the value a write
// would have stored. A missing row is still nil, nil.
func TestNullETagReadsAsDataMD5(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_d_nulletag")
	cal := &storage.CalDAV{DB: db}
	card := &storage.CardDAV{DB: db}
	calID, err := cal.CreateCalendar(t.Context(), "principals/admin", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	bookID, err := card.CreateAddressBook(t.Context(), "principals/admin", "contacts", nil)
	if err != nil {
		t.Fatal(err)
	}
	ics := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTART:20260101T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	vcf := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:x\r\nFN:X\r\nEND:VCARD\r\n")
	objETag, err := cal.CreateCalendarObject(t.Context(), calID, "x.ics", ics)
	if err != nil {
		t.Fatal(err)
	}
	cardETag, err := card.CreateCard(t.Context(), bookID, "x.vcf", vcf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE calendarobjects SET etag = NULL; UPDATE cards SET etag = NULL`); err != nil {
		t.Fatal(err)
	}

	obj, err := cal.CalendarObject(t.Context(), calID, "x.ics")
	if err != nil || obj == nil || obj.ETag != objETag {
		t.Errorf("CalendarObject etag = %+v, %v; want %s", obj, err, objETag)
	}
	if objs, err := cal.CalendarObjects(t.Context(), calID); err != nil || len(objs) != 1 || objs[0].ETag != objETag {
		t.Errorf("CalendarObjects = %+v, %v; want etag %s", objs, err, objETag)
	}
	c, err := card.Card(t.Context(), bookID, "x.vcf")
	if err != nil || c == nil || c.ETag != cardETag {
		t.Errorf("Card etag = %+v, %v; want %s", c, err, cardETag)
	}
	if cards, err := card.Cards(t.Context(), bookID); err != nil || len(cards) != 1 || cards[0].ETag != cardETag {
		t.Errorf("Cards = %+v, %v; want etag %s", cards, err, cardETag)
	}

	if obj, err := cal.CalendarObject(t.Context(), calID, "missing.ics"); obj != nil || err != nil {
		t.Errorf("CalendarObject(missing) = %+v, %v; want nil, nil", obj, err)
	}
	if c, err := card.Card(t.Context(), bookID, "missing.vcf"); c != nil || err != nil {
		t.Errorf("Card(missing) = %+v, %v; want nil, nil", c, err)
	}
	if p, err := (&storage.Principals{DB: db}).PrincipalByPath(t.Context(), "principals/missing"); p != nil || err != nil {
		t.Errorf("PrincipalByPath(missing) = %+v, %v; want nil, nil", p, err)
	}
}

// TestWritesToMissingRowsAreNotFound covers updates and deletes that matched
// no row (the object vanished between the caller's lookup and the write)
// still succeeding and logging a change: sync clients were told to fetch a
// uri that 404s, and the token moved for nothing.
func TestWritesToMissingRowsAreNotFound(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_d_missingrows")
	cal := &storage.CalDAV{DB: db}
	card := &storage.CardDAV{DB: db}

	calID, err := cal.CreateCalendar(t.Context(), "principals/admin", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	bookID, err := card.CreateAddressBook(t.Context(), "principals/admin", "contacts", nil)
	if err != nil {
		t.Fatal(err)
	}
	changes := func() (n int) {
		t.Helper()
		if err := db.QueryRow(`SELECT
			(SELECT count(*) FROM calendarchanges) + (SELECT count(*) FROM addressbookchanges) +
			(SELECT sum(synctoken) FROM calendars) + (SELECT sum(synctoken) FROM addressbooks)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := changes()

	ics := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\nDTSTART:20260101T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	vcf := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:x\r\nFN:X\r\nEND:VCARD\r\n")
	for name, err := range map[string]error{
		"UpdateCalendarObject": func() error { _, err := cal.UpdateCalendarObject(t.Context(), calID, "gone.ics", ics); return err }(),
		"DeleteCalendarObject": cal.DeleteCalendarObject(t.Context(), calID, "gone.ics"),
		"UpdateCard":           func() error { _, err := card.UpdateCard(t.Context(), bookID, "gone.vcf", vcf); return err }(),
		"DeleteCard":           card.DeleteCard(t.Context(), bookID, "gone.vcf"),
	} {
		if !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("%s on a missing row = %v, want ErrNotFound", name, err)
		}
	}
	// A PROPPATCH on an instance that is gone fails rather than logging.
	pp := dav.NewPropPatch(map[xml.Name]any{{Space: "DAV:", Local: "displayname"}: "Renamed"})
	cal.UpdateCalendar(t.Context(), caldav.CalendarID{Calendar: calID.Calendar, Instance: "999999"}, pp)
	if _, ok := pp.Commit(); ok {
		t.Error("UpdateCalendar on a missing instance succeeded")
	}
	if after := changes(); after != before {
		t.Errorf("change rows + tokens moved %d → %d for writes that matched nothing", before, after)
	}
}
