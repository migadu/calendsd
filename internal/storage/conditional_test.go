package storage_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"calendsd/internal/caldav"
	"calendsd/internal/storage"
)

// UpdateCalendarObjectIfMatch folds the ETag check into the UPDATE predicate
// (audit 4.3): a stale tag changes nothing and reports the mismatch.
func TestUpdateCalendarObjectIfMatch(t *testing.T) {
	root := repoRoot(t)
	if reason := pgOnlyPrereqsMissing(); reason != "" {
		t.Skipf("skipping conditional update test: %s", reason)
	}
	const dbName = "calserver_condupdate"
	run(t, root, "dropdb", "--if-exists", dbName)
	run(t, root, "createdb", dbName)
	for _, f := range mustGlob(t, filepath.Join(root, "dav/examples/sql/pgsql.*.sql")) {
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-f", f)
	}
	run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbName, "-c",
		`INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
		 INSERT INTO calendarinstances (calendarid, principaluri, access, uri)
		 VALUES (currval('calendars_id_seq'), 'principals/admin', 1, 'default')`)
	db, err := storage.Open(fmt.Sprintf("postgres:///%s?host=%s", dbName, os.Getenv("PGHOST")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var calID int64
	if err := db.QueryRow(`SELECT id FROM calendars LIMIT 1`).Scan(&calID); err != nil {
		t.Fatal(err)
	}
	backend := &storage.CalDAV{DB: db}
	id := caldav.CalendarID{Calendar: strconv.FormatInt(calID, 10)}
	var _ caldav.ConditionalUpdater = backend
	ctx := context.Background()

	ev := func(summary string) string {
		return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:c\r\nSUMMARY:" + summary +
			"\r\nDTSTART:20260715T120000Z\r\nDTEND:20260715T130000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	}
	v1, err := backend.CreateCalendarObject(ctx, id, "c.ics", []byte(ev("v1")))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := backend.UpdateCalendarObjectIfMatch(ctx, id, "c.ics", []byte(ev("v2")), v1)
	if err != nil {
		t.Fatalf("update with the current tag: %v", err)
	}
	if _, err := backend.UpdateCalendarObjectIfMatch(ctx, id, "c.ics", []byte(ev("v3")), v1); !errors.Is(err, caldav.ErrETagMismatch) {
		t.Fatalf("update with a stale tag: err = %v, want ErrETagMismatch", err)
	}
	obj, err := backend.CalendarObject(ctx, id, "c.ics")
	if err != nil || obj == nil {
		t.Fatal(err)
	}
	if obj.QuotedETag() != v2 || !containsLine(string(obj.CalendarData), "SUMMARY:v2") {
		t.Errorf("stale update changed the object: etag %s data:\n%s", obj.QuotedETag(), obj.CalendarData)
	}
	if _, err := backend.UpdateCalendarObjectIfMatch(ctx, id, "gone.ics", []byte(ev("x")), v1); !errors.Is(err, caldav.ErrETagMismatch) {
		t.Errorf("update of a missing object: err = %v, want ErrETagMismatch", err)
	}
}

func containsLine(data, line string) bool {
	for _, l := range splitLines(data) {
		if l == line {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			l := s[start:i]
			if len(l) > 0 && l[len(l)-1] == '\r' {
				l = l[:len(l)-1]
			}
			out = append(out, l)
			start = i + 1
		}
	}
	return append(out, s[start:])
}
