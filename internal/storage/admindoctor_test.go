package storage_test

// Doctor scan-cap tests against a scratch Postgres with the sabre schema
// plus the calserver extension tables. Postgres-only; skips when the dev
// Postgres isn't reachable.

import (
	"fmt"
	"testing"

	"calserver/internal/storage"
)

// TestDoctorScanCapInSQL pins the scan cap at its boundaries now that the
// doctor's queries stop one row past it in SQL (they used to stream the
// whole table and break in Go): exactly cap rows is a complete scan, one
// more is reported truncated, for objects and cards alike.
func TestDoctorScanCapInSQL(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_d_doctorcap")
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
	const n = 3
	for i := range n {
		ics := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:e%d\r\nDTSTART:20260101T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", i)
		if _, err := cal.CreateCalendarObject(calID, fmt.Sprintf("e%d.ics", i), []byte(ics)); err != nil {
			t.Fatal(err)
		}
		vcf := fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:c%d\r\nFN:C%d\r\nEND:VCARD\r\n", i, i)
		if _, err := card.CreateCard(bookID, fmt.Sprintf("c%d.vcf", i), []byte(vcf)); err != nil {
			t.Fatal(err)
		}
	}

	q := &storage.AdminQueries{DB: db}
	for _, tc := range []struct {
		cap, scanned int
		truncated    bool
	}{
		{n, n, false},
		{n - 1, n - 1, true},
	} {
		report, err := q.Doctor(tc.cap)
		if err != nil {
			t.Fatal(err)
		}
		if report.ObjectsScanned != tc.scanned || report.CardsScanned != tc.scanned || report.Truncated != tc.truncated {
			t.Errorf("Doctor(%d): objects=%d cards=%d truncated=%t, want %d/%d/%t",
				tc.cap, report.ObjectsScanned, report.CardsScanned, report.Truncated,
				tc.scanned, tc.scanned, tc.truncated)
		}
	}

	// The in-data searches take the same cap; a small table is never truncated.
	objs, truncated, err := q.Objects(storage.AdminObjectQuery{
		CalendarID: calID.Calendar, Match: "VEVENT", InData: true})
	if err != nil || len(objs) != n || truncated {
		t.Errorf("Objects in-data = %d rows, truncated=%t, err=%v; want %d, false", len(objs), truncated, err, n)
	}
	cards, truncated, err := q.CardsSearch(bookID, "VCARD", true, 0)
	if err != nil || len(cards) != n || truncated {
		t.Errorf("CardsSearch in-data = %d rows, truncated=%t, err=%v; want %d, false", len(cards), truncated, err, n)
	}
}
