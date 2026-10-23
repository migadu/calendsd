package storage_test

// Exercises the CalendarQuery SQL prefilter (component type + occurrence
// bounds, NULL-safe) against real Postgres. Skips when the dev DB is absent.

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"calendsd/internal/caldav"
	"calendsd/internal/storage"
	"calendsd/internal/storage/storagetest"
)

func TestCalendarQueryPrefilter(t *testing.T) {
	const dbName = "calserver_prefilter"
	db := storagetest.NewDB(t, dbName)
	mustExec(t, db,
		`INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
		 INSERT INTO calendarinstances (calendarid, principaluri, access, uri)
		 VALUES (currval('calendars_id_seq'), 'principals/admin', 1, 'default')`)

	var calID int64
	if err := db.QueryRow(`SELECT id FROM calendars LIMIT 1`).Scan(&calID); err != nil {
		t.Fatal(err)
	}
	backend := &storage.CalDAV{DB: db}
	id := caldav.CalendarID{Calendar: strconv.FormatInt(calID, 10)}

	// A July VEVENT (occurrence bounds set) and a VTODO (bounds NULL).
	julyEvent := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:e\r\nDTSTART:20260715T120000Z\r\nDTEND:20260715T130000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	todo := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:t\r\nDUE:20260715T120000Z\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	if _, err := backend.CreateCalendarObject(t.Context(), id, "event.ics", []byte(julyEvent)); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CreateCalendarObject(t.Context(), id, "todo.ics", []byte(todo)); err != nil {
		t.Fatal(err)
	}

	d := func(mon int) time.Time { return time.Date(2026, time.Month(mon), 1, 0, 0, 0, 0, time.UTC) }
	var zero time.Time
	far := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		comp       string
		start, end time.Time
		want       []string
	}{
		{"event in july range", "VEVENT", d(7), d(8), []string{"event.ics"}},
		{"event outside august range", "VEVENT", d(8), d(9), nil},
		{"todo by type, no range", "VTODO", zero, zero, []string{"todo.ics"}},
		{"todo passes range via NULL bounds", "VTODO", d(7), d(8), []string{"todo.ics"}},
		{"no component constraint", "", zero, zero, []string{"event.ics", "todo.ics"}},
		// Bounds past the int4 occurrence columns' range must compare, not
		// fail the query.
		{"range ending past 2038", "VEVENT", d(7), far, []string{"event.ics"}},
		{"range starting past 2038", "VEVENT", far, far.AddDate(1, 0, 0), nil},
	}
	for _, tc := range cases {
		got, err := backend.CalendarQuery(t.Context(), id, tc.comp, tc.start, tc.end)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !equalStrings(got, tc.want) {
			t.Errorf("%s: CalendarQuery = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The admin object browser takes the same bounds.
	farUnix := far.Unix()
	objs, _, err := (&storage.AdminQueries{DB: db}).Objects(t.Context(), storage.AdminObjectQuery{
		CalendarID: id.Calendar, End: &farUnix})
	if err != nil {
		t.Fatalf("Objects with a bound past 2038: %v", err)
	}
	if len(objs) != 2 {
		t.Errorf("Objects with a bound past 2038 = %d rows, want 2", len(objs))
	}
}

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	files, _ := filepath.Glob(pattern)
	if len(files) == 0 {
		t.Fatalf("no files match %s", pattern)
	}
	return files
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
