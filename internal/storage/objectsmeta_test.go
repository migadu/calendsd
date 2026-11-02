package storage_test

// The metadata-only object listing (docs/slow-queries.md, item 3): the same
// rows as the full listing minus the payload, one query that never touches
// calendardata, and the etag backfill that lets it skip the md5 fallback.

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/caldav"
	"github.com/migadu/calendsd/internal/schema"
	"github.com/migadu/calendsd/internal/server"
	"github.com/migadu/calendsd/internal/storage"
	"github.com/migadu/calendsd/internal/storage/storagetest"
)

const metaEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:%s\r\nDTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260601T100000Z\r\nDTEND:20260601T110000Z\r\nSUMMARY:Meta %s\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// metaCalendar seeds bob with one calendar holding n objects written through
// the backend, so etag and size are what production rows carry. It returns
// the calendar's ID.
func metaCalendar(t *testing.T, db *sql.DB, n int) caldav.CalendarID {
	t.Helper()
	mustExec(t, db, `
		INSERT INTO principals (uri, email, displayname) VALUES
			('principals/bob', 'bob@example.org', 'Bob');
		INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT');
		INSERT INTO calendarinstances (calendarid, principaluri, access, uri)
			VALUES (currval('calendars_id_seq'), 'principals/bob', 1, 'default');`)
	var id string
	if err := db.QueryRow(`SELECT max(id) FROM calendars`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	calID := caldav.CalendarID{Calendar: id}
	backend := &storage.CalDAV{DB: db}
	for i := range n {
		uid := "ev" + strconv.Itoa(i)
		data := []byte(strings.ReplaceAll(metaEvent, "%s", uid))
		if _, err := backend.CreateCalendarObject(context.Background(), calID, uid+".ics", data); err != nil {
			t.Fatal(err)
		}
	}
	return calID
}

func TestCalendarObjectsMetaMatchesFull(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_objects_meta")
	calID := metaCalendar(t, db, 5)
	backend := &storage.CalDAV{DB: db}

	full, err := backend.CalendarObjects(t.Context(), calID)
	if err != nil {
		t.Fatal(err)
	}
	qlog.Reset()
	meta, err := backend.CalendarObjectsMeta(t.Context(), calID)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != len(full) || len(full) != 5 {
		t.Fatalf("meta %d objects, full %d, want 5", len(meta), len(full))
	}
	for i := range full {
		if meta[i].URI != full[i].URI || meta[i].ETag != full[i].ETag || meta[i].Size != full[i].Size || meta[i].Component != full[i].Component {
			t.Errorf("row %d: meta %+v differs from full %+v", i, meta[i], full[i])
		}
		if meta[i].CalendarData != nil {
			t.Errorf("row %d: meta carries data", i)
		}
		if full[i].Size != int64(len(full[i].CalendarData)) {
			t.Errorf("row %d: stored size %d, payload %d bytes", i, full[i].Size, len(full[i].CalendarData))
		}
	}
	stmts := qlog.Statements()
	if len(stmts) != 1 {
		t.Fatalf("meta listing ran %d queries: %q", len(stmts), stmts)
	}
	if strings.Contains(stmts[0], "calendardata") {
		t.Errorf("meta listing reads calendardata: %s", stmts[0])
	}
}

// Migration 0004 fills in the etag of rows that lack one with the md5 the
// read path used to compute on every read.
func TestObjectEtagBackfill(t *testing.T) {
	db := storagetest.NewDB(t, "calserver_b_objects_etag_backfill")
	mustExec(t, db, `
		INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT');
		INSERT INTO calendarobjects (calendardata, uri, calendarid, lastmodified, etag, size, componenttype)
			VALUES ('BEGIN:VCALENDAR END:VCALENDAR', 'legacy.ics', currval('calendars_id_seq'), 0, NULL, 29, 'VEVENT'),
			       ('BEGIN:VCALENDAR END:VCALENDAR', 'kept.ics', currval('calendars_id_seq'), 0, 'keep', 29, 'VEVENT');`)
	var backfill string
	for _, m := range schema.Migrations() {
		if m.Version == 4 {
			backfill = m.SQL
		}
	}
	if backfill == "" {
		t.Fatal("migration 0004 not found")
	}
	mustExec(t, db, backfill)

	sum := md5.Sum([]byte("BEGIN:VCALENDAR END:VCALENDAR"))
	for uri, want := range map[string]string{"legacy.ics": hex.EncodeToString(sum[:]), "kept.ics": "keep"} {
		var etag sql.NullString
		if err := db.QueryRow(`SELECT etag FROM calendarobjects WHERE uri = $1`, uri).Scan(&etag); err != nil {
			t.Fatal(err)
		}
		if etag.String != want {
			t.Errorf("%s: etag = %q, want %q", uri, etag.String, want)
		}
	}
}

// Through the whole stack: a Depth:1 PROPFIND for getetag and
// getcontentlength on a calendar reads calendarobjects once, without the
// payload, and a multiget of an object it listed still returns the data.
func TestPropfindListsCalendarWithoutPayload(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_objects_propfind")
	metaCalendar(t, db, 5)
	srv := server.NewLoggedIn(db, server.Config{BaseURI: "/"}, "principals/bob")
	do := func(method, path, body string) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Depth", "1")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 207 {
			t.Fatalf("%s %s = %d:\n%s", method, path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	qlog.Reset()
	body := do("PROPFIND", "/calendars/bob/default/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/><d:getcontentlength/></d:prop></d:propfind>`)
	if n := strings.Count(body, "<d:getetag>"); n != 5 {
		t.Errorf("%d getetag values, want 5:\n%s", n, body)
	}
	var objectQueries []string
	for _, s := range qlog.Statements() {
		if strings.Contains(s, "FROM calendarobjects") {
			objectQueries = append(objectQueries, s)
		}
		if strings.Contains(s, "calendardata") {
			t.Errorf("PROPFIND read calendardata: %s", s)
		}
	}
	if len(objectQueries) != 1 {
		t.Errorf("PROPFIND ran %d calendarobjects queries, want 1: %q", len(objectQueries), objectQueries)
	}
	if total := qlog.Count(); total > 15 {
		t.Errorf("PROPFIND ran %d queries in all for 5 objects, want a handful: %q", total, qlog.Statements())
	}

	body = do("REPORT", "/calendars/bob/default/",
		`<?xml version="1.0"?><c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`+
			`<d:prop><c:calendar-data/></d:prop><d:href>/calendars/bob/default/ev1.ics</d:href></c:calendar-multiget>`)
	if !strings.Contains(body, "SUMMARY:Meta ev1") {
		t.Errorf("multiget after the listing lacks the payload:\n%s", body)
	}
}
