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

	"calserver/internal/server"
	"calserver/internal/storage"
)

const (
	parityDB     = "calserver_parity"
	sabreAddr    = "127.0.0.1:8123"
	testICS      = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//parity//test//EN\r\nBEGIN:VEVENT\r\nUID:parity-1\r\nDTSTAMP:20251201T000000Z\r\nDTSTART;TZID=Europe/Berlin:20260601T100000\r\nDTEND;TZID=Europe/Berlin:20260601T110000\r\nRRULE:FREQ=WEEKLY;COUNT=4\r\nSUMMARY:Parity event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	goWrittenICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//parity//test//EN\r\nBEGIN:VEVENT\r\nUID:parity-2\r\nDTSTAMP:20251201T000000Z\r\nDTSTART:20260702T120000Z\r\nDTEND:20260702T130000Z\r\nSUMMARY:Written by Go\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
)

func TestParity(t *testing.T) {
	if os.Getenv("PARITY") == "" {
		t.Skip("set PARITY=1 to run the live co-existence parity test")
	}
	root := repoRoot(t)

	// Fresh database from sabre's schema files.
	run(t, root, "dropdb", "--if-exists", parityDB)
	run(t, root, "createdb", parityDB)
	sqlFiles, _ := filepath.Glob(filepath.Join(root, "dav/examples/sql/pgsql.*.sql"))
	if len(sqlFiles) == 0 {
		t.Fatal("no schema files found")
	}
	for _, f := range sqlFiles {
		run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", parityDB, "-f", f)
	}
	run(t, root, "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", parityDB, "-c",
		`INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
		 INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri)
		 VALUES (currval('calendars_id_seq'), 'principals/admin', 1, 'parity calendar', 'default')`)

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
	goSrv := httptest.NewServer(server.New(db, server.Config{BaseURI: "/"}))
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

func extractCTag(body string) string {
	m := regexp.MustCompile(`getctag>([^<]+)<`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
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
