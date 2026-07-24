package dav

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// PROPFIND with <propname/> returns the names of existing properties with
// empty values and status 200 — not their values (RFC 4918 §9.1).
func TestPropfindPropName(t *testing.T) {
	s := NewServer("/", testRoot())
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:propname/></d:propfind>`
	req := httptest.NewRequest("PROPFIND", "/", strings.NewReader(body))
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != 207 {
		t.Fatalf("PROPFIND propname = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "<d:resourcetype/>") {
		t.Errorf("propname did not emit a bare resourcetype name:\n%s", out)
	}
	if strings.Contains(out, "<d:collection") {
		t.Errorf("propname leaked property values (resourcetype content present):\n%s", out)
	}
	if strings.Contains(out, "404") {
		t.Errorf("propname must not include 404 propstats:\n%s", out)
	}
	if !strings.Contains(out, "200 OK") {
		t.Errorf("propname must report a 200 status:\n%s", out)
	}
}
