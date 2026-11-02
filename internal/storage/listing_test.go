package storage_test

// The two full listings docs/slow-queries.md is about: each is one query and
// each reports its duration under its own op label.

import (
	"testing"

	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/metrics"
	"github.com/migadu/calendsd/internal/storage"
	"github.com/migadu/calendsd/internal/storage/storagetest"
)

func TestListingsAreTimedAndSingleQuery(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_listing_metrics")
	mustExec(t, db, `
		INSERT INTO principals (uri, email, displayname) VALUES
			('principals/bob', 'bob@example.org', 'Bob');
		INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT');
		INSERT INTO calendarinstances (calendarid, principaluri, access, uri)
			VALUES (currval('calendars_id_seq'), 'principals/bob', 1, 'default');
		INSERT INTO calendarobjects (calendardata, uri, calendarid, lastmodified, etag, size, componenttype, uid) VALUES
			('BEGIN:VCALENDAR END:VCALENDAR', 'a.ics', currval('calendars_id_seq'), 0, 'e1', 29, 'VEVENT', 'a'),
			('BEGIN:VCALENDAR END:VCALENDAR', 'b.ics', currval('calendars_id_seq'), 0, 'e2', 29, 'VEVENT', 'b');
	`)
	var calendarID string
	if err := db.QueryRow(`SELECT max(id) FROM calendars`).Scan(&calendarID); err != nil {
		t.Fatal(err)
	}
	qlog.Reset()

	set := metrics.New("test")
	principals := &storage.Principals{DB: db, Metrics: set}
	calendars := &storage.CalDAV{DB: db, Metrics: set}

	list, err := principals.PrincipalsByPrefix(t.Context(), "principals")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 { // admin from the seed, bob
		t.Errorf("PrincipalsByPrefix returned %d principals, want 2", len(list))
	}
	objs, err := calendars.CalendarObjects(t.Context(), caldav.CalendarID{Calendar: calendarID})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Errorf("CalendarObjects returned %d objects, want 2", len(objs))
	}
	if n := qlog.Count(); n != 2 {
		t.Errorf("the two listings ran %d queries, want 2: %q", n, qlog.Statements())
	}

	ops := dbOps(t, set)
	for _, op := range []string{"principals_by_prefix", "calendar_objects"} {
		if !ops[op] {
			t.Errorf("db_op_duration has no %q series; have %v", op, ops)
		}
	}
}

// dbOps returns the op labels calserver_db_op_duration_seconds has recorded.
func dbOps(t *testing.T, set *metrics.Set) map[string]bool {
	t.Helper()
	families, err := set.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "calserver_db_op_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "op" {
					ops[l.GetValue()] = true
				}
			}
		}
	}
	return ops
}
