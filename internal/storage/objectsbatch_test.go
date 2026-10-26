package storage_test

// Batched object fetches (docs/slow-queries.md, item 4): a calendar-query
// and a multiget over many objects of one calendar read calendarobjects
// once, not once per object, and answer in the same order as before.

import (
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"calendsd/internal/caldav"
	"calendsd/internal/server"
	"calendsd/internal/storage"
	"calendsd/internal/storage/storagetest"
)

func TestQueryObjectsIsBatchedAndOrdered(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_objects_batch_query")
	metaCalendar(t, db, 200)
	backend := &storage.CalDAV{DB: db}
	infos, err := backend.CalendarsForUser(t.Context(), "principals/bob")
	if err != nil || len(infos) != 1 {
		t.Fatalf("CalendarsForUser: %v, %d calendars", err, len(infos))
	}
	cal := caldav.NewCalendar(backend, infos[0])

	// The per-URI path's order is the prefilter's order.
	uris, err := backend.CalendarQuery(t.Context(), infos[0].ID, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	qlog.Reset()
	objs, err := cal.QueryObjects(t.Context(), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if n := qlog.Count(); n > 2 {
		t.Errorf("QueryObjects over 200 objects ran %d queries, want at most 2", n)
	}
	got := make([]string, len(objs))
	for i, obj := range objs {
		got[i] = obj.Name()
	}
	if !slices.Equal(got, uris) || len(got) != 200 {
		t.Errorf("QueryObjects returned %d objects in a different order than the per-URI path", len(got))
	}
}

func reportQueries(t *testing.T, qlog *storagetest.QueryLog) (objects, total int) {
	t.Helper()
	for _, s := range qlog.Statements() {
		if strings.Contains(s, "FROM calendarobjects") {
			objects++
		}
	}
	return objects, qlog.Count()
}

// A multiget of 200 hrefs in one calendar resolves the calendar once and
// fetches its objects with one query; an href with no object still gets its
// 404 entry.
func TestMultigetIsBatched(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_objects_batch_multiget")
	metaCalendar(t, db, 200)
	srv := server.NewLoggedIn(db, server.Config{BaseURI: "/"}, "principals/bob")

	var hrefs strings.Builder
	for i := range 200 {
		hrefs.WriteString(`<d:href>/calendars/bob/default/ev` + strconv.Itoa(i) + `.ics</d:href>`)
	}
	hrefs.WriteString(`<d:href>/calendars/bob/default/missing.ics</d:href>`)
	body := `<?xml version="1.0"?><c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/><c:calendar-data/></d:prop>` + hrefs.String() + `</c:calendar-multiget>`

	qlog.Reset()
	req := httptest.NewRequest("REPORT", "/calendars/bob/default/", strings.NewReader(body))
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 207 {
		t.Fatalf("REPORT = %d:\n%s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if n := strings.Count(out, "SUMMARY:Meta ev"); n != 200 {
		t.Errorf("%d payloads in the multiget response, want 200", n)
	}
	if !strings.Contains(out, "missing.ics") || strings.Count(out, "HTTP/1.1 404") != 1 {
		t.Errorf("missing href is not answered 404 (%d 404s):\n%s", strings.Count(out, "HTTP/1.1 404"), out[:min(len(out), 600)])
	}
	objects, total := reportQueries(t, qlog)
	if objects != 1 {
		t.Errorf("multiget ran %d calendarobjects queries, want 1: %q", objects, qlog.Statements())
	}
	// One propertystorage read per object remains; everything else is per
	// request.
	if total > 200+20 {
		t.Errorf("multiget ran %d queries in all, want about one per object: %q", total, qlog.Statements())
	}
}

// A calendar-query matching 200 objects loads them with at most two
// calendarobjects queries and answers from the loaded nodes.
func TestCalendarQueryIsBatched(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_objects_batch_calquery")
	metaCalendar(t, db, 200)
	srv := server.NewLoggedIn(db, server.Config{BaseURI: "/"}, "principals/bob")

	body := `<?xml version="1.0"?><c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/><c:calendar-data/></d:prop>` +
		`<c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"/></c:comp-filter></c:filter>` +
		`</c:calendar-query>`

	qlog.Reset()
	req := httptest.NewRequest("REPORT", "/calendars/bob/default/", strings.NewReader(body))
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 207 {
		t.Fatalf("REPORT = %d:\n%s", w.Code, w.Body.String())
	}
	if n := strings.Count(w.Body.String(), "SUMMARY:Meta ev"); n != 200 {
		t.Errorf("%d payloads in the calendar-query response, want 200", n)
	}
	objects, total := reportQueries(t, qlog)
	if objects > 2 {
		t.Errorf("calendar-query ran %d calendarobjects queries, want at most 2: %q", objects, qlog.Statements())
	}
	if total > 200+20 {
		t.Errorf("calendar-query ran %d queries in all, want about one per object: %q", total, qlog.Statements())
	}
}
