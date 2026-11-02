package dav

// Phase 4 low items at the dav layer: 500 bodies (6.3), tagged-URI gating in
// the If header (6.4), COPY/MOVE onto an ancestor (6.19).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A plain error from a node must reach the client as a generic 500, with
// the driver-grade text kept for the log.
func TestInternalErrorBodyIsGeneric(t *testing.T) {
	de := AsError(errors.New(`pq: relation "calendarobjects" does not exist (host=db.internal)`))
	if de.Status != http.StatusInternalServerError || de.Message != "Internal Server Error" {
		t.Errorf("AsError = %d %q, want 500 Internal Server Error", de.Status, de.Message)
	}
	if de.Cause() == nil || !strings.Contains(de.Cause().Error(), "db.internal") {
		t.Error("cause not retained for the log")
	}
	root := newMemColl("root", &memFile{name: "a.txt", contents: []byte("x"), etag: `"a"`})
	s := NewServer("/", root)
	s.AddPlugin(erroringPlugin{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/a.txt", nil))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "db.internal") || !strings.Contains(rec.Body.String(), "Internal Server Error") {
		t.Errorf("500 body leaks or is wrong: %d\n%s", rec.Code, rec.Body.String())
	}
}

type erroringPlugin struct{}

func (erroringPlugin) PluginName() string { return "erroring" }
func (erroringPlugin) Init(*Server)       {}
func (erroringPlugin) BeforeMethod(http.ResponseWriter, *http.Request) (bool, error) {
	return false, errors.New(`pq: relation "x" does not exist (host=db.internal)`)
}

// denyGate refuses every tagged URI under /secret.
type denyGate struct{}

func (denyGate) PluginName() string { return "deny-gate" }
func (denyGate) Init(*Server)       {}
func (denyGate) MayReadTaggedURI(_ context.Context, path string) bool {
	return !strings.HasPrefix(path, "secret")
}

func TestIfHeaderTaggedURIIsGated(t *testing.T) {
	secret := &memFile{name: "s.txt", contents: []byte("s"), etag: `"secret-etag"`}
	root := newMemColl("root", newMemColl("secret", secret), &memFile{name: "mine.txt", contents: []byte("m"), etag: `"m"`})
	s := NewServer("/", root)
	s.AddPlugin(denyGate{})

	// Correct ETag of the forbidden resource in a tagged list: still 412,
	// so the header cannot be used to confirm the ETag.
	for _, ifHeader := range []string{`</secret/s.txt> (["secret-etag"])`, `</secret/s.txt> (Not ["wrong"])`} {
		req := httptest.NewRequest("PUT", "/mine.txt", strings.NewReader("new"))
		req.Header.Set("If", ifHeader)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusPreconditionFailed {
			t.Errorf("If %s = %d, want 412", ifHeader, rec.Code)
		}
	}
	// A readable tagged URI with its real ETag still works.
	req := httptest.NewRequest("PUT", "/mine.txt", strings.NewReader("new"))
	req.Header.Set("If", `</mine.txt> (["m"])`)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("If on own resource = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}

func TestCopyMoveOntoAncestorRefused(t *testing.T) {
	for _, method := range []string{"COPY", "MOVE"} {
		obj := &memFile{name: "e.ics", contents: []byte("e"), etag: `"e"`}
		home := newMemColl("home", obj)
		root := newMemColl("root", newMemColl("calendars", home))
		s := NewServer("/", root)
		req := httptest.NewRequest(method, "/calendars/home/e.ics", nil)
		req.Header.Set("Destination", "/calendars/home")
		req.Header.Set("Overwrite", "T")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s onto ancestor = %d, want 403: %s", method, rec.Code, rec.Body.String())
		}
		if !childExists(t, root, "calendars") || !childExists(t, home, "e.ics") {
			t.Errorf("%s onto ancestor destroyed the collection or the source", method)
		}
	}
}
