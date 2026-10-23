package storage_test

// The live co-existence parity test (PLAN.md §5): the Go server and a real
// sabre/dav instance run against the same Postgres database; every value one
// writes must be readable by the other. This is the acceptance gate for
// "runs alongside sabre-dav".
//
// Opt-in (needs the dev-shell toolchain: Postgres running via
// scripts/dev-db.sh, php, psql):
//
//	PARITY=1 go test ./internal/storage/ -run TestParity -v

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"calendsd/internal/auth"
	"calendsd/internal/schema"
	"calendsd/internal/server"
	"calendsd/internal/storage"
	"calendsd/internal/storage/storagetest"
)

const (
	parityDB     = "calserver_parity"
	sabreAddr    = "127.0.0.1:8123"
	testICS      = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//parity//test//EN\r\nBEGIN:VEVENT\r\nUID:parity-1\r\nDTSTAMP:20251201T000000Z\r\nDTSTART;TZID=Europe/Berlin:20260601T100000\r\nDTEND;TZID=Europe/Berlin:20260601T110000\r\nRRULE:FREQ=WEEKLY;COUNT=4\r\nSUMMARY:Parity event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	goWrittenICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//parity//test//EN\r\nBEGIN:VEVENT\r\nUID:parity-2\r\nDTSTAMP:20251201T000000Z\r\nDTSTART:20260702T120000Z\r\nDTEND:20260702T130000Z\r\nSUMMARY:Written by Go\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
)

func TestParity(t *testing.T) {
	root := repoRoot(t)
	// Opt-in only: sabre is no longer a rollback target, so co-existence
	// with it is not a contract any more. Kept for one release, then
	// removed with scripts/sabre-server.php.
	if os.Getenv("PARITY") == "" {
		t.Skip("skipping live parity test: set PARITY=1 to run it")
	}

	// Fresh database from sabre's schema files.
	run(t, root, "dropdb", "--if-exists", parityDB)
	run(t, root, "createdb", parityDB)
	sqlFiles, _ := filepath.Glob(filepath.Join(root, "dav/examples/sql/pgsql.*.sql"))
	if len(sqlFiles) == 0 {
		t.Fatal("no schema files found")
	}
	// The calendsd migrations on top: running them over sabre's own schema
	// and then sabre against the result proves sabre is unaffected by them.
	for _, f := range sqlFiles {
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", parityDB, "-f", f)
	}
	parityConn, err := storage.Open(storagetest.DSN(parityDB))
	if err != nil {
		t.Fatal(err)
	}
	_, err = schema.Apply(t.Context(), parityConn, 0)
	parityConn.Close()
	if err != nil {
		t.Fatal(err)
	}
	run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", parityDB, "-c",
		`INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
		 INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri)
		 VALUES (currval('calendars_id_seq'), 'principals/admin', 1, 'parity calendar', 'default');
		 INSERT INTO principals (uri, email, displayname) VALUES ('principals/user2', 'user2@example.org', 'User Two');
		 INSERT INTO users (username, digesta1) VALUES ('user2', 'unused');
		 INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
		 INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri)
		 VALUES (currval('calendars_id_seq'), 'principals/user2', 1, 'user2 calendar', 'default')`)

	// The reference sabre instance.
	sabre := exec.Command("php", "-S", sabreAddr, "scripts/sabre-server.php")
	sabre.Dir = root
	sabre.Env = append(os.Environ(), "PGDATABASE="+parityDB)
	if err := sabre.Start(); err != nil {
		t.Fatalf("starting sabre: %v", err)
	}
	t.Cleanup(func() { _ = sabre.Process.Kill() })
	waitFor(t, "http://"+sabreAddr+"/")

	// The Go server on the same database.
	pghost := os.Getenv("PGHOST")
	if pghost == "" {
		t.Fatal("PGHOST is not set (run inside the dev shell)")
	}
	db, err := storage.Open(fmt.Sprintf("postgres:///%s?host=%s", parityDB, pghost))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()
	goSrv := httptest.NewServer(server.Build(db, server.Config{BaseURI: "/"}, "").Handler)
	defer goSrv.Close()

	sabreClient := &digestClient{base: "http://" + sabreAddr, username: "admin", password: "admin"}
	goClient := &digestClient{base: goSrv.URL, username: "admin", password: "admin"}

	objectPath := "/calendars/admin/default/parity1.ics"

	t.Run("SabreWritesGoReads", func(t *testing.T) {
		status, _, headers := sabreClient.do(t, "PUT", objectPath, testICS, "Content-Type", "text/calendar")
		if status != 201 {
			t.Fatalf("sabre PUT status = %d", status)
		}
		sabreETag := headers.Get("ETag")

		status, body, goHeaders := goClient.do(t, "GET", objectPath, "")
		if status != 200 {
			t.Fatalf("Go GET status = %d, body %s", status, body)
		}
		if body != testICS {
			t.Errorf("Go read different bytes than sabre stored:\n%q\nvs\n%q", body, testICS)
		}
		if goETag := goHeaders.Get("ETag"); goETag != sabreETag {
			t.Errorf("ETag mismatch: go %s, sabre %s", goETag, sabreETag)
		}
	})

	t.Run("CrossCTags", func(t *testing.T) {
		propfind := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><cs:getctag xmlns:cs="http://calendarserver.org/ns/"/></d:prop></d:propfind>`
		_, sabreBody, _ := sabreClient.do(t, "PROPFIND", "/calendars/admin/default/", propfind, "Depth", "0", "Content-Type", "application/xml")
		_, goBody, _ := goClient.do(t, "PROPFIND", "/calendars/admin/default/", propfind, "Depth", "0", "Content-Type", "application/xml")
		sabreCTag, goCTag := extractCTag(sabreBody), extractCTag(goBody)
		if sabreCTag == "" || sabreCTag != goCTag {
			t.Errorf("ctag mismatch: go %q, sabre %q", goCTag, sabreCTag)
		}
	})

	t.Run("GoWritesSabreReads", func(t *testing.T) {
		// Capture sabre's sync token before the Go write.
		tokenBefore := sabreSyncToken(t, sabreClient)

		status, body, goHeaders := goClient.do(t, "PUT", "/calendars/admin/default/parity2.ics", goWrittenICS, "Content-Type", "text/calendar")
		if status != 201 {
			t.Fatalf("Go PUT status = %d, body %s", status, body)
		}
		sum := md5.Sum([]byte(goWrittenICS))
		if want := `"` + hex.EncodeToString(sum[:]) + `"`; goHeaders.Get("ETag") != want {
			t.Errorf("Go PUT ETag = %s, want %s", goHeaders.Get("ETag"), want)
		}

		status, body, sabreHeaders := sabreClient.do(t, "GET", "/calendars/admin/default/parity2.ics", "")
		if status != 200 {
			t.Fatalf("sabre GET of Go-written object: status %d, body %s", status, body)
		}
		if body != goWrittenICS {
			t.Errorf("sabre read different bytes than Go stored")
		}
		if sabreHeaders.Get("ETag") != goHeaders.Get("ETag") {
			t.Errorf("ETag mismatch on Go-written object: sabre %s, go %s",
				sabreHeaders.Get("ETag"), goHeaders.Get("ETag"))
		}

		// Sabre's sync REPORT must see the Go-written change: proves the
		// calendarchanges/synctoken convention holds cross-implementation.
		syncBody := fmt.Sprintf(`<?xml version="1.0"?><d:sync-collection xmlns:d="DAV:"><d:sync-token>%s</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`, tokenBefore)
		status, body, _ = sabreClient.do(t, "REPORT", "/calendars/admin/default/", syncBody, "Content-Type", "application/xml")
		if status != 207 {
			t.Fatalf("sabre sync REPORT status = %d, body %s", status, body)
		}
		if !strings.Contains(body, "parity2.ics") {
			t.Errorf("sabre sync REPORT does not list the Go-written object:\n%s", body)
		}
	})

	t.Run("SabreQueriesGoWrite", func(t *testing.T) {
		// A calendar-query on sabre must find the Go-written event via the
		// firstoccurence/lastoccurence prefilter Go computed on PUT.
		query := `<?xml version="1.0"?><c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="20260701T000000Z" end="20260703T000000Z"/></c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
		status, body, _ := sabreClient.do(t, "REPORT", "/calendars/admin/default/", query, "Depth", "1", "Content-Type", "application/xml")
		if status != 207 {
			t.Fatalf("sabre calendar-query status = %d", status)
		}
		if !strings.Contains(body, "parity2.ics") {
			t.Errorf("sabre calendar-query missed the Go-written event:\n%s", body)
		}
		if strings.Contains(body, "parity1.ics") {
			t.Errorf("sabre calendar-query wrongly matched the June event for a July range")
		}
	})

	t.Run("EventCRUDLifecycle", func(t *testing.T) {
		// A full create/read/update/delete of one event, alternating the
		// server that mutates and the server that observes, so every step of
		// the lifecycle is proven to cross the shared database. This mirrors
		// the real deployment: events authored in calclient (the Go server)
		// must appear — and stay in step through edits and deletion — for a
		// CalDAV client reading through sabre (e.g. Apple Calendar), and the
		// reverse. Each mutation also asserts the collection ctag advances,
		// which is the sole signal a polling client uses to re-sync.
		const (
			calBase   = "/calendars/admin/default/"
			crudPath  = "/calendars/admin/default/crud1.ics"
			origRange = `<c:time-range start="20270310T000000Z" end="20270311T000000Z"/>`
			newRange  = `<c:time-range start="20270311T000000Z" end="20270312T000000Z"/>`
		)
		orig := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//parity//test//EN\r\nBEGIN:VEVENT\r\nUID:crud-1\r\nDTSTAMP:20270101T000000Z\r\nDTSTART:20270310T090000Z\r\nDTEND:20270310T100000Z\r\nSUMMARY:CRUD original\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

		// query runs a VEVENT time-range calendar-query (the report a client
		// issues to populate a month/day view) and returns the multistatus.
		query := func(c *digestClient, timeRange string) string {
			t.Helper()
			q := `<?xml version="1.0"?><c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT">` + timeRange + `</c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
			status, body, _ := c.do(t, "REPORT", calBase, q, "Depth", "1", "Content-Type", "application/xml")
			if status != 207 {
				t.Fatalf("calendar-query status = %d: %s", status, body)
			}
			return body
		}

		// CREATE through the Go server (the calclient authoring path).
		ctag0 := ctagOf(t, sabreClient, calBase)
		status, body, goHeaders := goClient.do(t, "PUT", crudPath, orig, "Content-Type", "text/calendar")
		if status != 201 {
			t.Fatalf("Go create PUT = %d: %s", status, body)
		}
		createdETag := goHeaders.Get("ETag")

		// READ back through sabre: exact bytes, matching ETag, and — the
		// property that actually fills a client's calendar view — the
		// time-range calendar-query must return it.
		status, body, sabreHeaders := sabreClient.do(t, "GET", crudPath, "")
		if status != 200 || body != orig {
			t.Fatalf("sabre GET of Go-created event = %d:\n%q", status, body)
		}
		if sabreHeaders.Get("ETag") != createdETag {
			t.Errorf("create ETag mismatch: go %s, sabre %s", createdETag, sabreHeaders.Get("ETag"))
		}
		if !strings.Contains(query(sabreClient, origRange), "crud1.ics") {
			t.Error("sabre calendar-query missed the Go-created event (a client's view would be empty)")
		}
		ctag1 := ctagOf(t, sabreClient, calBase)
		if ctag1 == ctag0 {
			t.Errorf("sabre ctag did not advance after Go create (still %q) — a polling client never re-syncs", ctag1)
		}

		// UPDATE through sabre (the client-side edit path): new summary, and a
		// new start time that lands in a different day-range.
		updated := strings.ReplaceAll(orig, "CRUD original", "CRUD updated")
		updated = strings.ReplaceAll(updated, "20270310T09", "20270311T14")
		updated = strings.ReplaceAll(updated, "20270310T10", "20270311T15")
		status, body, sabreHeaders = sabreClient.do(t, "PUT", crudPath, updated, "Content-Type", "text/calendar")
		if status != 204 && status != 201 {
			t.Fatalf("sabre update PUT = %d: %s", status, body)
		}
		updatedETag := sabreHeaders.Get("ETag")
		if updatedETag == createdETag {
			t.Error("ETag did not change after update")
		}

		// READ the update back through the Go server: new bytes, new ETag,
		// and the occurrence prefilter must have moved with it — found in the
		// new range, absent from the old one.
		status, body, goHeaders = goClient.do(t, "GET", crudPath, "")
		if status != 200 || body != updated {
			t.Fatalf("Go GET of sabre-updated event = %d:\n%q", status, body)
		}
		if goHeaders.Get("ETag") != updatedETag {
			t.Errorf("update ETag mismatch: sabre %s, go %s", updatedETag, goHeaders.Get("ETag"))
		}
		if !strings.Contains(query(goClient, newRange), "crud1.ics") {
			t.Error("Go calendar-query missed the updated event in its new time range")
		}
		if strings.Contains(query(goClient, origRange), "crud1.ics") {
			t.Error("Go calendar-query still matches the old range after update (stale firstoccurence/lastoccurence)")
		}
		if ctag2 := ctagOf(t, sabreClient, calBase); ctag2 == ctag1 {
			t.Errorf("sabre ctag did not advance after the update (still %q)", ctag2)
		}

		// DELETE through the Go server, then prove the removal crosses to
		// sabre three ways: GET 404, calendar-query no longer matches, and an
		// incremental sync-collection reports the href as 404.
		tokenBeforeDelete := sabreSyncToken(t, sabreClient)
		status, body, _ = goClient.do(t, "DELETE", crudPath, "")
		if status != 204 {
			t.Fatalf("Go DELETE = %d: %s", status, body)
		}
		if st, _, _ := sabreClient.do(t, "GET", crudPath, ""); st != 404 {
			t.Errorf("sabre GET after Go delete = %d, want 404", st)
		}
		if strings.Contains(query(sabreClient, newRange), "crud1.ics") {
			t.Error("sabre calendar-query still returns the deleted event")
		}
		syncBody := fmt.Sprintf(`<?xml version="1.0"?><d:sync-collection xmlns:d="DAV:"><d:sync-token>%s</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`, tokenBeforeDelete)
		status, body, _ = sabreClient.do(t, "REPORT", calBase, syncBody, "Content-Type", "application/xml")
		if status != 207 {
			t.Fatalf("sabre sync REPORT after delete = %d: %s", status, body)
		}
		if !strings.Contains(body, "crud1.ics") || !strings.Contains(body, "404") {
			t.Errorf("sabre incremental sync did not report the deletion:\n%s", body)
		}
	})

	t.Run("SabreWriteVisibleInGoSync", func(t *testing.T) {
		// The Go server's own sync-collection REPORT must interoperate
		// with sabre-issued tokens and sabre-written changes.
		tokenBefore := sabreSyncToken(t, sabreClient)
		status, _, _ := sabreClient.do(t, "PUT", "/calendars/admin/default/parity3.ics",
			strings.ReplaceAll(goWrittenICS, "parity-2", "parity-3"), "Content-Type", "text/calendar")
		if status != 201 {
			t.Fatalf("sabre PUT status = %d", status)
		}
		syncBody := fmt.Sprintf(`<?xml version="1.0"?><d:sync-collection xmlns:d="DAV:"><d:sync-token>%s</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`, tokenBefore)
		status, body, _ := goClient.do(t, "REPORT", "/calendars/admin/default/", syncBody, "Content-Type", "application/xml")
		if status != 207 {
			t.Fatalf("Go sync REPORT status = %d, body %s", status, body)
		}
		if !strings.Contains(body, "parity3.ics") {
			t.Errorf("Go sync REPORT does not list the sabre-written object:\n%s", body)
		}
		// Both servers must agree on the resulting token.
		goToken := regexp.MustCompile(`<d:sync-token>([^<]+)</d:sync-token>`).FindStringSubmatch(body)
		if goToken == nil || goToken[1] != sabreSyncToken(t, sabreClient) {
			t.Errorf("Go sync token %v does not match sabre's %s", goToken, sabreSyncToken(t, sabreClient))
		}
	})

	t.Run("PropertyParity", func(t *testing.T) {
		// Column-backed property: Go PROPPATCH → calendarinstances →
		// sabre PROPFIND.
		patch := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:a="http://apple.com/ns/ical/"><d:set><d:prop><a:calendar-color>#FF0000FF</a:calendar-color></d:prop></d:set></d:propertyupdate>`
		status, body, _ := goClient.do(t, "PROPPATCH", "/calendars/admin/default/", patch, "Content-Type", "application/xml")
		if status != 207 || strings.Contains(body, "403") {
			t.Fatalf("Go PROPPATCH color = %d: %s", status, body)
		}
		find := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:a="http://apple.com/ns/ical/"><d:prop><a:calendar-color/></d:prop></d:propfind>`
		_, body, _ = sabreClient.do(t, "PROPFIND", "/calendars/admin/default/", find, "Depth", "0", "Content-Type", "application/xml")
		if !strings.Contains(body, "#FF0000FF") {
			t.Errorf("sabre does not see the Go-written calendar-color:\n%s", body)
		}

		// Dead property: Go PROPPATCH → propertystorage (valuetype 1) →
		// sabre PROPFIND.
		patch = `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:x="http://example.org/ns"><d:set><d:prop><x:go-written>from-go</x:go-written></d:prop></d:set></d:propertyupdate>`
		status, body, _ = goClient.do(t, "PROPPATCH", "/calendars/admin/default/", patch, "Content-Type", "application/xml")
		if status != 207 || strings.Contains(body, "403") {
			t.Fatalf("Go PROPPATCH dead prop = %d: %s", status, body)
		}
		find = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:x="http://example.org/ns"><d:prop><x:go-written/></d:prop></d:propfind>`
		_, body, _ = sabreClient.do(t, "PROPFIND", "/calendars/admin/default/", find, "Depth", "0", "Content-Type", "application/xml")
		if !strings.Contains(body, "from-go") {
			t.Errorf("sabre does not see the Go-written dead property:\n%s", body)
		}

		// Reverse: sabre PROPPATCH dead prop → Go PROPFIND.
		patch = `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:x="http://example.org/ns"><d:set><d:prop><x:sabre-written>from-sabre</x:sabre-written></d:prop></d:set></d:propertyupdate>`
		status, body, _ = sabreClient.do(t, "PROPPATCH", "/calendars/admin/default/", patch, "Content-Type", "application/xml")
		if status != 207 && status != 204 {
			t.Fatalf("sabre PROPPATCH = %d: %s", status, body)
		}
		find = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:x="http://example.org/ns"><d:prop><x:sabre-written/></d:prop></d:propfind>`
		_, body, _ = goClient.do(t, "PROPFIND", "/calendars/admin/default/", find, "Depth", "0", "Content-Type", "application/xml")
		if !strings.Contains(body, "from-sabre") {
			t.Errorf("Go does not see the sabre-written dead property:\n%s", body)
		}
	})

	t.Run("SchedulingParity", func(t *testing.T) {
		// An invite written through the Go server must be delivered so
		// sabre sees both the attendee's calendar copy and the inbox item.
		invite := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//parity//test//EN\r\nBEGIN:VEVENT\r\nUID:parity-invite-1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260801T100000Z\r\nDTEND:20260801T110000Z\r\nSUMMARY:Planning\r\nORGANIZER;CN=Admin:mailto:admin@example.org\r\nATTENDEE;CN=User Two;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:user2@example.org\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		status, body, _ := goClient.do(t, "PUT", "/calendars/admin/default/invite1.ics", invite, "Content-Type", "text/calendar")
		if status != 201 {
			t.Fatalf("Go PUT invite = %d: %s", status, body)
		}

		// The organizer's stored copy carries the delivery status.
		status, body, _ = sabreClient.do(t, "GET", "/calendars/admin/default/invite1.ics", "")
		if status != 200 || !strings.Contains(body, "SCHEDULE-STATUS=1.2") {
			t.Errorf("sabre-read organizer copy lacks SCHEDULE-STATUS=1.2 (%d):\n%s", status, body)
		}

		// The attendee copy is visible to sabre.
		var attendeeCopies int
		if err := db.QueryRow(`
			SELECT count(*) FROM calendarobjects o
			  JOIN calendarinstances i ON i.calendarid = o.calendarid
			 WHERE i.principaluri = 'principals/user2' AND o.uid = 'parity-invite-1'`).Scan(&attendeeCopies); err != nil {
			t.Fatal(err)
		}
		if attendeeCopies != 1 {
			t.Errorf("attendee calendar copies = %d, want 1", attendeeCopies)
		}
		var inboxItems int
		if err := db.QueryRow(
			`SELECT count(*) FROM schedulingobjects WHERE principaluri = 'principals/user2'`).Scan(&inboxItems); err != nil {
			t.Fatal(err)
		}
		if inboxItems != 1 {
			t.Errorf("inbox items = %d, want 1", inboxItems)
		}

		// Reverse: a sabre-written invite is delivered where the Go server
		// reads it.
		invite2 := strings.ReplaceAll(strings.ReplaceAll(invite, "parity-invite-1", "parity-invite-2"), "20260801", "20260802")
		status, body, _ = sabreClient.do(t, "PUT", "/calendars/admin/default/invite2.ics", invite2, "Content-Type", "text/calendar")
		if status != 201 {
			t.Fatalf("sabre PUT invite = %d: %s", status, body)
		}
		query := `<?xml version="1.0"?><c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-data/></d:prop><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="20260802T000000Z" end="20260803T000000Z"/></c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`
		status, body, _ = goClient.do(t, "REPORT", "/calendars/user2/default/", query, "Depth", "1", "Content-Type", "application/xml")
		if status != 207 || !strings.Contains(body, "parity-invite-2") {
			t.Errorf("Go calendar-query on the attendee calendar missed the sabre-delivered invite (%d):\n%s", status, body)
		}
	})

	t.Run("CtagAdvancesOnMutations", func(t *testing.T) {
		// The collection ctag is the only signal a polling client (iOS,
		// Thunderbird) uses to decide a calendar changed and needs a re-sync.
		// Object create/update/delete are covered by EventCRUDLifecycle;
		// these lock down the *other* mutation paths, so a future write that
		// forgets to bump synctoken fails here instead of silently stalling a
		// client until some unrelated change happens to nudge the ctag.
		//
		// (The schedule inbox is deliberately not checked: like sabre's
		// Schedule\Inbox it exposes no getctag/sync-token — clients poll it
		// directly — so the meaningful scheduling assertion is that the
		// attendee's *calendar* copy advances that calendar's ctag.)
		const (
			adminCal   = "/calendars/admin/default/"
			archiveCal = "/calendars/admin/archive/"
			user2Cal   = "/calendars/user2/default/"
		)

		// (1) A calendar metadata PROPPATCH (rename/recolor) must advance the
		// ctag — sabre records an empty-uri change for exactly this — and both
		// servers must then report the same new value.
		before := ctagOf(t, goClient, adminCal)
		patch := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:"><d:set><d:prop><d:displayname>Renamed default</d:displayname></d:prop></d:set></d:propertyupdate>`
		if status, body, _ := goClient.do(t, "PROPPATCH", adminCal, patch, "Content-Type", "application/xml"); status != 207 || strings.Contains(body, "403") {
			t.Fatalf("Go PROPPATCH displayname = %d: %s", status, body)
		}
		goCtag := ctagOf(t, goClient, adminCal)
		if goCtag == before {
			t.Errorf("ctag did not advance after metadata PROPPATCH (still %q)", goCtag)
		}
		if sabreCtag := ctagOf(t, sabreClient, adminCal); sabreCtag != goCtag {
			t.Errorf("post-PROPPATCH ctag mismatch: go %q, sabre %q", goCtag, sabreCtag)
		}

		// (2) MOVE of an object between two calendars must advance BOTH the
		// source and destination ctags — a client watching either collection
		// has to notice.
		mk := `<?xml version="1.0"?><c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop><d:displayname>Archive</d:displayname></d:prop></d:set></c:mkcalendar>`
		if status, body, _ := goClient.do(t, "MKCALENDAR", archiveCal, mk, "Content-Type", "application/xml"); status != 201 {
			t.Fatalf("Go MKCALENDAR archive = %d: %s", status, body)
		}
		moveICS := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//parity//test//EN\r\nBEGIN:VEVENT\r\nUID:parity-move\r\nDTSTAMP:20270101T000000Z\r\nDTSTART:20270401T090000Z\r\nDTEND:20270401T100000Z\r\nSUMMARY:Move me\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		if status, body, _ := goClient.do(t, "PUT", adminCal+"movesrc.ics", moveICS, "Content-Type", "text/calendar"); status != 201 {
			t.Fatalf("Go PUT move source = %d: %s", status, body)
		}
		srcBefore := ctagOf(t, goClient, adminCal)
		dstBefore := ctagOf(t, goClient, archiveCal)
		status, body, _ := goClient.do(t, "MOVE", adminCal+"movesrc.ics", "",
			"Destination", goClient.base+archiveCal+"movedst.ics")
		if status != 201 {
			t.Fatalf("Go MOVE = %d: %s", status, body)
		}
		if after := ctagOf(t, goClient, adminCal); after == srcBefore {
			t.Errorf("source ctag did not advance after MOVE-out (still %q)", after)
		}
		if after := ctagOf(t, goClient, archiveCal); after == dstBefore {
			t.Errorf("destination ctag did not advance after MOVE-in (still %q)", after)
		}
		if st, _, _ := goClient.do(t, "GET", adminCal+"movesrc.ics", ""); st != 404 {
			t.Errorf("source object still present after MOVE = %d, want 404", st)
		}
		if st, _, _ := sabreClient.do(t, "GET", archiveCal+"movedst.ics", ""); st != 200 {
			t.Errorf("sabre cannot GET the moved object at its destination = %d", st)
		}

		// (3) A scheduling delivery drops a copy into the attendee's calendar;
		// that calendar's ctag must advance so the attendee's client re-syncs
		// and sees the new invitation.
		u2Before := ctagOf(t, goClient, user2Cal)
		invite := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//parity//test//EN\r\nBEGIN:VEVENT\r\nUID:parity-ctag-invite\r\nDTSTAMP:20270101T000000Z\r\nDTSTART:20270501T100000Z\r\nDTEND:20270501T110000Z\r\nSUMMARY:Invite bump\r\nORGANIZER;CN=Admin:mailto:admin@example.org\r\nATTENDEE;CN=User Two;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:user2@example.org\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		if status, body, _ := goClient.do(t, "PUT", adminCal+"ctag-invite.ics", invite, "Content-Type", "text/calendar"); status != 201 {
			t.Fatalf("Go PUT invite = %d: %s", status, body)
		}
		if after := ctagOf(t, goClient, user2Cal); after == u2Before {
			t.Errorf("attendee calendar ctag did not advance after scheduling delivery (still %q) — the invite would not sync to the attendee's client", after)
		}
	})

	t.Run("CardDAVParity", func(t *testing.T) {
		// A vCard written through the Go server must be readable, with an
		// identical ETag, by sabre — over the shared cards table.
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", parityDB, "-c",
			`INSERT INTO addressbooks (principaluri, uri, displayname, synctoken)
			 VALUES ('principals/admin', 'contacts', 'Contacts', 1)`)
		card := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:parity-card-1\r\nFN:Parity Person\r\nEMAIL:p@example.org\r\nEND:VCARD\r\n"
		status, body, goHeaders := goClient.do(t, "PUT", "/addressbooks/admin/contacts/p1.vcf", card, "Content-Type", "text/vcard")
		if status != 201 {
			t.Fatalf("Go PUT vcard = %d: %s", status, body)
		}
		status, body, sabreHeaders := sabreClient.do(t, "GET", "/addressbooks/admin/contacts/p1.vcf", "")
		if status != 200 || body != card {
			t.Errorf("sabre read a different vcard (%d):\n%q", status, body)
		}
		if sabreHeaders.Get("ETag") != goHeaders.Get("ETag") {
			t.Errorf("vcard ETag mismatch: go %s, sabre %s", goHeaders.Get("ETag"), sabreHeaders.Get("ETag"))
		}
		// Reverse: sabre-written card found by Go's addressbook-query.
		card2 := strings.ReplaceAll(card, "parity-card-1", "parity-card-2")
		card2 = strings.ReplaceAll(card2, "Parity Person", "Query Target")
		status, _, _ = sabreClient.do(t, "PUT", "/addressbooks/admin/contacts/p2.vcf", card2, "Content-Type", "text/vcard")
		if status != 201 {
			t.Fatalf("sabre PUT vcard = %d", status)
		}
		q := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav"><d:prop><d:getetag/></d:prop><c:filter><c:prop-filter name="FN"><c:text-match match-type="contains">Query Target</c:text-match></c:prop-filter></c:filter></c:addressbook-query>`
		status, body, _ = goClient.do(t, "REPORT", "/addressbooks/admin/contacts/", q, "Depth", "1", "Content-Type", "application/xml")
		if status != 207 || !strings.Contains(body, "p2.vcf") {
			t.Errorf("Go addressbook-query missed the sabre-written card (%d):\n%s", status, body)
		}
	})

	t.Run("GoDeleteVisibleInSabreSync", func(t *testing.T) {
		tokenBefore := sabreSyncToken(t, sabreClient)
		status, body, _ := goClient.do(t, "DELETE", "/calendars/admin/default/parity2.ics", "")
		if status != 204 {
			t.Fatalf("Go DELETE status = %d, body %s", status, body)
		}
		syncBody := fmt.Sprintf(`<?xml version="1.0"?><d:sync-collection xmlns:d="DAV:"><d:sync-token>%s</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`, tokenBefore)
		status, body, _ = sabreClient.do(t, "REPORT", "/calendars/admin/default/", syncBody, "Content-Type", "application/xml")
		if status != 207 {
			t.Fatalf("sabre sync REPORT status = %d", status)
		}
		if !strings.Contains(body, "parity2.ics") || !strings.Contains(body, "404") {
			t.Errorf("sabre sync REPORT does not report the Go-side deletion:\n%s", body)
		}
	})

	t.Run("AdminMadeUserWorksInSabre", func(t *testing.T) {
		// Phase C acceptance (ADMIN-PLAN.md §5): a user created through
		// the admin carve-out — digesta1 minted from the realm, principal
		// triple inserted, default calendar auto-provisioned through the
		// production code path — must be fully usable via real sabre.
		writes := &storage.AdminWrites{DB: db}
		if err := writes.CreateUser(t.Context(), "adminmade",
			auth.DigestA1("adminmade", "SabreDAV", "s3cret"),
			"adminmade@example.org", "Admin Made"); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		// Provision the default calendar the way the panel does: a home
		// PROPFIND through the auto-provisioning composition.
		prov := server.NewLoggedIn(db,
			server.Config{BaseURI: "/", AutoProvision: true}, "principals/adminmade")
		propfindBody := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`
		req := httptest.NewRequest("PROPFIND", "/calendars/adminmade/", strings.NewReader(propfindBody))
		req.Header.Set("Depth", "1")
		req.Header.Set("Content-Type", "application/xml")
		rec := httptest.NewRecorder()
		prov.ServeHTTP(rec, req)
		if rec.Code != 207 {
			t.Fatalf("provisioning PROPFIND = %d: %s", rec.Code, rec.Body.String())
		}

		// The minted digesta1 must pass sabre's Digest auth, and sabre
		// must list the provisioned calendar.
		newClient := &digestClient{base: "http://" + sabreAddr, username: "adminmade", password: "s3cret"}
		status, body, _ := newClient.do(t, "PROPFIND", "/calendars/adminmade/", propfindBody,
			"Depth", "1", "Content-Type", "application/xml")
		if status != 207 {
			t.Fatalf("sabre PROPFIND as admin-made user = %d: %s", status, body)
		}
		// Auto-provision creates the Calendar-Server default URI "calendar".
		if !strings.Contains(body, "/calendars/adminmade/calendar/") {
			t.Errorf("sabre does not list the provisioned default calendar:\n%s", body)
		}

		// An admin PROPPATCH (panel path: production plugin chain as the
		// admin principal) must be visible through sabre.
		adm := server.NewLoggedIn(db, server.Config{BaseURI: "/"}, "principals/admin")
		patch := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:"><d:set><d:prop><d:displayname>Renamed by admin</d:displayname></d:prop></d:set></d:propertyupdate>`
		req = httptest.NewRequest("PROPPATCH", "/calendars/adminmade/calendar/", strings.NewReader(patch))
		req.Header.Set("Content-Type", "application/xml")
		rec = httptest.NewRecorder()
		adm.ServeHTTP(rec, req)
		if rec.Code != 207 || !strings.Contains(rec.Body.String(), "200 OK") {
			t.Fatalf("admin PROPPATCH = %d: %s", rec.Code, rec.Body.String())
		}
		nameFind := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>`
		status, body, _ = newClient.do(t, "PROPFIND", "/calendars/adminmade/calendar/", nameFind,
			"Depth", "0", "Content-Type", "application/xml")
		if status != 207 || !strings.Contains(body, "Renamed by admin") {
			t.Errorf("sabre does not see the admin-set displayname (%d):\n%s", status, body)
		}
	})
}

func sabreSyncToken(t *testing.T, c *digestClient) string {
	t.Helper()
	propfind := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:sync-token/></d:prop></d:propfind>`
	_, body, _ := c.do(t, "PROPFIND", "/calendars/admin/default/", propfind, "Depth", "0", "Content-Type", "application/xml")
	m := regexp.MustCompile(`<d:sync-token>([^<]+)</d:sync-token>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no sync-token in PROPFIND response: %s", body)
	}
	return m[1]
}

// ctagOf reads the CalendarServer getctag of a collection — the value a
// polling client (iOS, Thunderbird) compares to decide whether to re-sync.
func ctagOf(t *testing.T, c *digestClient, path string) string {
	t.Helper()
	propfind := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><cs:getctag xmlns:cs="http://calendarserver.org/ns/"/></d:prop></d:propfind>`
	_, body, _ := c.do(t, "PROPFIND", path, propfind, "Depth", "0", "Content-Type", "application/xml")
	return extractCTag(body)
}

func extractCTag(body string) string {
	m := regexp.MustCompile(`getctag>([^<]+)<`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// parityPrereqsMissing reports why the live harness can't run here: it
// needs php with the sabre vendor tree, the psql tooling, and the dev
// Postgres from scripts/dev-db.sh.
func parityPrereqsMissing(root string) string {
	for _, bin := range []string{"php", "psql", "createdb", "dropdb"} {
		if _, err := exec.LookPath(bin); err != nil {
			return bin + " not on PATH"
		}
	}
	if _, err := os.Stat(filepath.Join(root, "dav", "vendor", "autoload.php")); err != nil {
		return "dav/vendor missing (run composer install in dav/)"
	}
	if os.Getenv("PGHOST") == "" {
		return "PGHOST not set"
	}
	if err := exec.Command("pg_isready", "-h", os.Getenv("PGHOST")).Run(); err != nil {
		return "postgres not reachable (run scripts/dev-db.sh start)"
	}
	return ""
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/storage -> repo root
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func waitFor(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server at %s did not come up", url)
}

// digestClient is a minimal RFC 2617 Digest (qop=auth) HTTP client, enough
// to talk to both servers with the shared admin credentials.
type digestClient struct {
	base, username, password string
}

func (c *digestClient) do(t *testing.T, method, path, body string, headerPairs ...string) (int, string, http.Header) {
	t.Helper()
	// First request to collect the challenge.
	resp := c.request(t, method, path, body, "", headerPairs)
	if resp.StatusCode != http.StatusUnauthorized {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	authz := c.answer(t, challenge, method, path)
	resp = c.request(t, method, path, body, authz, headerPairs)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (c *digestClient) request(t *testing.T, method, path, body, authz string, headerPairs []string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headerPairs); i += 2 {
		req.Header.Set(headerPairs[i], headerPairs[i+1])
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (c *digestClient) answer(t *testing.T, challenge, method, path string) string {
	t.Helper()
	get := func(key string) string {
		m := regexp.MustCompile(key + `="?([^",]+)"?`).FindStringSubmatch(challenge)
		if m == nil {
			return ""
		}
		return m[1]
	}
	realm, nonce, opaque := get("realm"), get("nonce"), get("opaque")
	if realm == "" || nonce == "" {
		t.Fatalf("unusable digest challenge: %s", challenge)
	}
	const nc, cnonce = "00000001", "abcdef01"
	h := func(s string) string {
		sum := md5.Sum([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	a1 := h(c.username + ":" + realm + ":" + c.password)
	a2 := h(method + ":" + path)
	response := h(strings.Join([]string{a1, nonce, nc, cnonce, "auth", a2}, ":"))
	authz := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
		c.username, realm, nonce, path, nc, cnonce, response)
	if opaque != "" {
		authz += fmt.Sprintf(`, opaque="%s"`, opaque)
	}
	return authz
}
