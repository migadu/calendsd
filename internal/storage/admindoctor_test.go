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
	calID, err := cal.CreateCalendar(t.Context(), "principals/admin", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	bookID, err := card.CreateAddressBook(t.Context(), "principals/admin", "contacts", nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 3
	for i := range n {
		ics := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:e%d\r\nDTSTART:20260101T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", i)
		if _, err := cal.CreateCalendarObject(t.Context(), calID, fmt.Sprintf("e%d.ics", i), []byte(ics)); err != nil {
			t.Fatal(err)
		}
		vcf := fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:c%d\r\nFN:C%d\r\nEND:VCARD\r\n", i, i)
		if _, err := card.CreateCard(t.Context(), bookID, fmt.Sprintf("c%d.vcf", i), []byte(vcf)); err != nil {
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
		report, err := q.Doctor(t.Context(), tc.cap)
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
	objs, truncated, err := q.Objects(t.Context(), storage.AdminObjectQuery{
		CalendarID: calID.Calendar, Match: "VEVENT", InData: true})
	if err != nil || len(objs) != n || truncated {
		t.Errorf("Objects in-data = %d rows, truncated=%t, err=%v; want %d, false", len(objs), truncated, err, n)
	}
	cards, truncated, err := q.CardsSearch(t.Context(), bookID, "VCARD", true, 0)
	if err != nil || len(cards) != n || truncated {
		t.Errorf("CardsSearch in-data = %d rows, truncated=%t, err=%v; want %d, false", len(cards), truncated, err, n)
	}
}

// TestDoctorIMipPendingErrors covers the stale-iMIP check swallowing every
// error from the jobs query, including a failed scan or read: the doctor then
// reported nothing stale and a clean run. Only a missing jobs table (a
// queue-less database) may be skipped.
func TestDoctorIMipPendingErrors(t *testing.T) {
	db := openContactsScratchDB(t, "calserver_d_doctorimip")
	cal := &storage.CalDAV{DB: db}
	calID, err := cal.CreateCalendar(t.Context(), "principals/admin", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:pending-1\r\nDTSTART:20260101T100000Z\r\n" +
		"ORGANIZER:mailto:admin@example.org\r\n" +
		"ATTENDEE;SCHEDULE-STATUS=\"1.0;Scheduling message is queued for delivery\":mailto:bob@example.org\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, err := cal.CreateCalendarObject(t.Context(), calID, "pending.ics", []byte(ics)); err != nil {
		t.Fatal(err)
	}
	q := &storage.AdminQueries{DB: db}
	stale := func(r *storage.AdminDoctorReport) (n int) {
		for _, f := range r.Findings {
			if f.Check == "imip-pending-stale" {
				n++
			}
		}
		return n
	}

	report, err := q.Doctor(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if stale(report) != 1 {
		t.Fatalf("pending object with no job: %d imip-pending-stale findings, want 1", stale(report))
	}

	// A broken jobs query is an error, not a clean report.
	if _, err := db.Exec(`ALTER TABLE jobs RENAME COLUMN args TO args_gone`); err != nil {
		t.Fatal(err)
	}
	if report, err := q.Doctor(t.Context(), 0); err == nil {
		t.Fatalf("Doctor with a broken jobs query succeeded (%d stale findings); want an error", stale(report))
	}

	// No jobs table at all: skipped silently.
	if _, err := db.Exec(`DROP TABLE jobs CASCADE`); err != nil {
		t.Fatal(err)
	}
	report, err = q.Doctor(t.Context(), 0)
	if err != nil {
		t.Fatalf("Doctor without a jobs table: %v", err)
	}
	if stale(report) != 0 {
		t.Fatalf("Doctor without a jobs table flagged %d object(s)", stale(report))
	}
}
