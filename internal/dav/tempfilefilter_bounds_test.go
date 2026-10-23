package dav

// Audit 5.4: the temp-file store must cap what it accepts and key files by
// principal, so one user cannot read or delete another's temp file at the
// same path.

import (
	"net/http"
	"strings"
	"testing"
)

// loggedIn stands in for the auth plugin: it sets a fixed principal on every
// request, the way auth.NewLoggedInPlugin does (not importable from here).
type loggedIn struct{ principal string }

func (loggedIn) PluginName() string { return "logged-in" }
func (loggedIn) Init(*Server)       {}
func (l loggedIn) BeforeMethod(_ http.ResponseWriter, r *http.Request) (bool, error) {
	SetPrincipal(r.Context(), l.principal)
	return true, nil
}

// tempServerAs builds a temp-filter server over dir authenticated as principal.
func tempServerAs(dir, principal string) *Server {
	s := NewServer("/", testRoot())
	s.AddPlugin(loggedIn{principal})
	s.AddPlugin(&TempFileFilterPlugin{DataDir: dir})
	return s
}

func TestTempFileSizeCap(t *testing.T) {
	s := newTempFilterServer(t)
	big := strings.Repeat("x", maxTempFileSize+1)
	rec := do(t, s, "PUT", "/calendars/._big", big, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized temp PUT = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/calendars/._big", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("oversized temp file was stored: GET = %d", rec.Code)
	}
	if rec := do(t, s, "PUT", "/calendars/._ok", strings.Repeat("x", 1024), nil); rec.Code != http.StatusCreated {
		t.Errorf("small temp PUT = %d, want 201", rec.Code)
	}
}

func TestTempFilesAreKeyedByPrincipal(t *testing.T) {
	dir := t.TempDir()
	alice, bob := tempServerAs(dir, "principals/alice"), tempServerAs(dir, "principals/bob")

	if rec := do(t, alice, "PUT", "/calendars/.DS_Store", "alice's", nil); rec.Code != http.StatusCreated {
		t.Fatalf("alice PUT = %d", rec.Code)
	}
	for _, m := range []string{"GET", "DELETE"} {
		if rec := do(t, bob, m, "/calendars/.DS_Store", "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("bob %s of alice's temp file = %d, want 404", m, rec.Code)
		}
	}
	if rec := do(t, alice, "GET", "/calendars/.DS_Store", "", nil); rec.Code != http.StatusOK || rec.Body.String() != "alice's" {
		t.Errorf("alice GET = %d %q, want her own file", rec.Code, rec.Body.String())
	}
}
