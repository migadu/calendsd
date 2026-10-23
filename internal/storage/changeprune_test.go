package storage_test

// Change-log retention against a scratch Postgres: what PruneChangeLogs
// removes, the floor it leaves, and how a sync REPORT answers either side of
// it. Skips when the dev Postgres isn't reachable.

import (
	"strconv"
	"testing"
	"time"

	"calendsd/internal/caldav"
	"calendsd/internal/storage"
	"calendsd/internal/storage/storagetest"
)

func TestPruneChangeLogs(t *testing.T) {
	db := storagetest.NewDB(t, "calserver_changeprune")
	// Calendar 1: tokens 1–5 are old (3 twice, as rows that share a token),
	// 6–10 recent. Calendar 2: only old rows. Calendar 3: an old row at its
	// current token: the floor rises to it, the row stays, and the prune
	// still finishes. Address book 1 like calendar 2.
	mustExec(t, db, `
		INSERT INTO calendars (id, synctoken, components) VALUES (1, 11, 'VEVENT'), (2, 4, 'VEVENT'), (3, 5, 'VEVENT');
		INSERT INTO calendarchanges (uri, synctoken, calendarid, operation, created)
		SELECT 'a' || t || '.ics', t, 1, 2, now() - interval '100 days' FROM generate_series(1, 5) t;
		INSERT INTO calendarchanges (uri, synctoken, calendarid, operation, created)
		VALUES ('b3.ics', 3, 1, 2, now() - interval '100 days');
		INSERT INTO calendarchanges (uri, synctoken, calendarid, operation, created)
		SELECT 'a' || t || '.ics', t, 1, 2, now() - interval '1 day' FROM generate_series(6, 10) t;
		INSERT INTO calendarchanges (uri, synctoken, calendarid, operation, created)
		SELECT 'c' || t || '.ics', t, 2, 1, now() - interval '100 days' FROM generate_series(1, 3) t;
		INSERT INTO calendarchanges (uri, synctoken, calendarid, operation, created)
		VALUES ('d.ics', 5, 3, 1, now() - interval '100 days');
		INSERT INTO addressbooks (id, principaluri, uri, synctoken) VALUES (1, 'principals/admin', 'contacts', 3);
		INSERT INTO addressbookchanges (uri, synctoken, addressbookid, operation, created)
		SELECT 'v' || t || '.vcf', t, 1, 1, now() - interval '100 days' FROM generate_series(1, 2) t;
	`)
	ctx := t.Context()
	cutoff := time.Now().Add(-90 * 24 * time.Hour)

	n, err := storage.PruneChangeLogs(ctx, db, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6+3+2 {
		t.Errorf("removed %d rows, want 11", n)
	}
	floor := func(table string, id int) int {
		t.Helper()
		var f int
		if err := db.QueryRow(`SELECT synctoken_floor FROM `+table+` WHERE id = $1`, id).Scan(&f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, c := range []struct {
		table string
		id    int
		want  int
	}{{"calendars", 1, 6}, {"calendars", 2, 4}, {"calendars", 3, 5}, {"addressbooks", 1, 3}} {
		if got := floor(c.table, c.id); got != c.want {
			t.Errorf("%s %d floor = %d, want %d", c.table, c.id, got, c.want)
		}
	}
	if n, err := storage.PruneChangeLogs(ctx, db, cutoff); err != nil || n != 0 {
		t.Errorf("second prune removed %d, %v; want nothing", n, err)
	}

	cal := &storage.CalDAV{DB: db}
	changes := func(id, token int) []string {
		t.Helper()
		cs, err := cal.ChangesForCalendar(ctx, caldav.CalendarID{Calendar: strconv.Itoa(id)}, strconv.Itoa(token), 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if cs == nil {
			return nil
		}
		return append([]string{"ok"}, cs.Modified...)
	}
	// A token at the floor still gets a delta; one below it is refused.
	if got := changes(1, 6); len(got) != 6 {
		t.Errorf("token at the floor: %v, want the 5 recent changes", got)
	}
	if got := changes(1, 5); got != nil {
		t.Errorf("token below the floor: %v, want invalid-sync-token", got)
	}
	if got := changes(2, 4); got == nil {
		t.Error("current token of a fully pruned calendar refused, want an empty delta")
	}
	if got := changes(2, 3); got != nil {
		t.Errorf("token below a fully pruned calendar's floor: %v, want invalid-sync-token", got)
	}

	card := &storage.CardDAV{DB: db}
	if cs, err := card.ChangesForAddressBook(ctx, "1", "2", 1, 0); err != nil || cs != nil {
		t.Errorf("address book token below the floor = %v, %v; want invalid-sync-token", cs, err)
	}
}
