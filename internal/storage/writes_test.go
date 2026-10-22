package storage_test

// Write-path regression tests for the CalDAV and CardDAV backends against a
// scratch Postgres with the sabre schema. Postgres-only; skips when the dev
// Postgres isn't reachable.

import (
	"encoding/xml"
	"errors"
	"testing"

	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/storage"
)

// TestWritesToMissingRowsAreNotFound covers updates and deletes that matched
// no row (the object vanished between the caller's lookup and the write)
// still succeeding and logging a change: sync clients were told to fetch a
// uri that 404s, and the token moved for nothing.
func TestWritesToMissingRowsAreNotFound(t *testing.T) {
	db := openCardDAVScratchDB(t, "calserver_d_missingrows")
	cal := &storage.CalDAV{DB: db}
	card := &storage.CardDAV{DB: db}

	calID, err := cal.CreateCalendar("principals/admin", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	bookID, err := card.CreateAddressBook("principals/admin", "contacts", nil)
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
		"UpdateCalendarObject": func() error { _, err := cal.UpdateCalendarObject(calID, "gone.ics", ics); return err }(),
		"DeleteCalendarObject": cal.DeleteCalendarObject(calID, "gone.ics"),
		"UpdateCard":           func() error { _, err := card.UpdateCard(bookID, "gone.vcf", vcf); return err }(),
		"DeleteCard":           card.DeleteCard(bookID, "gone.vcf"),
	} {
		if !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("%s on a missing row = %v, want ErrNotFound", name, err)
		}
	}
	// A PROPPATCH on an instance that is gone fails rather than logging.
	pp := dav.NewPropPatch(map[xml.Name]any{{Space: "DAV:", Local: "displayname"}: "Renamed"})
	cal.UpdateCalendar(caldav.CalendarID{Calendar: calID.Calendar, Instance: "999999"}, pp)
	if _, ok := pp.Commit(); ok {
		t.Error("UpdateCalendar on a missing instance succeeded")
	}
	if after := changes(); after != before {
		t.Errorf("change rows + tokens moved %d → %d for writes that matched nothing", before, after)
	}
}
