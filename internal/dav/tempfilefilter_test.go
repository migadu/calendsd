package dav

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTempFilterServer returns a server whose only extra plugin is the temp
// filter, backed by a per-test scratch directory.
func newTempFilterServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer("/", testRoot())
	s.AddPlugin(&TempFileFilterPlugin{DataDir: t.TempDir()})
	return s
}

func do(t *testing.T, s *Server, method, uri, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, uri, nil)
	} else {
		r = httptest.NewRequest(method, uri, strings.NewReader(body))
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

func TestTempFileFilterRoundTrip(t *testing.T) {
	s := newTempFilterServer(t)
	const uri = "/calendars/admin/._resourcefork"

	// PUT creates the file: 201 + marker header, never touching the backend
	// (whose parent collection does not even resolve).
	rec := do(t, s, "PUT", uri, "junk", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT (new) status = %d, want 201", rec.Code)
	}
	if rec.Header().Get("X-Sabre-Temp") != "true" {
		t.Errorf("PUT missing X-Sabre-Temp: true header")
	}

	// GET reads it straight back.
	rec = do(t, s, "GET", uri, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "junk" {
		t.Errorf("GET body = %q, want %q", got, "junk")
	}
	if rec.Header().Get("X-Sabre-Temp") != "true" {
		t.Errorf("GET missing X-Sabre-Temp header")
	}

	// PROPFIND reports the lazy fixed property set as a 207.
	rec = do(t, s, "PROPFIND", uri, "", nil)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND status = %d, want 207", rec.Code)
	}
	for _, want := range []string{"tempFile", "getcontentlength", "getlastmodified"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("PROPFIND body missing %q:\n%s", want, rec.Body.String())
		}
	}

	// PUT over an existing temp file is a 200 (sabre's status choice).
	rec = do(t, s, "PUT", uri, "junk2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (replace) status = %d, want 200", rec.Code)
	}

	// DELETE removes it: 204.
	rec = do(t, s, "DELETE", uri, "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", rec.Code)
	}

	// After deletion the name falls through to normal handling, which 404s the
	// nonexistent node instead of the filter serving a stale copy.
	rec = do(t, s, "GET", uri, "", nil)
	if rec.Code == http.StatusOK {
		t.Errorf("GET after DELETE served a temp file; want fall-through 404, got 200")
	}
	if rec.Header().Get("X-Sabre-Temp") == "true" {
		t.Errorf("GET after DELETE still marked X-Sabre-Temp")
	}
}

func TestTempFileFilterIfNoneMatch(t *testing.T) {
	s := newTempFilterServer(t)
	const uri = "/x/.DS_Store"

	if rec := do(t, s, "PUT", uri, "a", nil); rec.Code != http.StatusCreated {
		t.Fatalf("initial PUT status = %d, want 201", rec.Code)
	}
	// If-None-Match on an existing temp file must fail the precondition (412),
	// like sabre's httpPut.
	rec := do(t, s, "PUT", uri, "b", map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("PUT If-None-Match status = %d, want 412", rec.Code)
	}
}

func TestTempFileFilterIgnoresNormalNames(t *testing.T) {
	s := newTempFilterServer(t)
	// A regular calendar-object name is not a temp file, so the filter leaves
	// it entirely alone (no marker header, handled by the core/backend path).
	rec := do(t, s, "PUT", "/calendars/admin/event.ics", "BEGIN:VCALENDAR", nil)
	if rec.Header().Get("X-Sabre-Temp") == "true" {
		t.Errorf("normal filename was intercepted by the temp filter")
	}
}

func TestTempFileMatch(t *testing.T) {
	p := &TempFileFilterPlugin{DataDir: t.TempDir()}
	p.Init(NewServer("/", testRoot()))
	temp := []string{
		"a/b/._fork", ".DS_Store", "dir/.DS_Store", "desktop.ini",
		"Thumbs.db", ".event.ics.swp", ".dat123", "~lock.doc#",
	}
	normal := []string{"event.ics", "contact.vcf", "calendars/admin/", "foo.swp", "DS_Store"}
	for _, path := range temp {
		if _, ok := p.tempLocation(path); !ok {
			t.Errorf("tempLocation(%q) = not-temp, want temp", path)
		}
	}
	for _, path := range normal {
		if _, ok := p.tempLocation(path); ok {
			t.Errorf("tempLocation(%q) = temp, want not-temp", path)
		}
	}
}
