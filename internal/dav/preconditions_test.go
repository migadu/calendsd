package dav

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Bug: checkPreconditions returns early after a non-matching If-None-Match,
// skipping If-Unmodified-Since (sabre only suppresses If-Modified-Since).
func TestPreconditionIfNoneMatchDoesNotSuppressIfUnmodifiedSince(t *testing.T) {
	mtime := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	file := &memFile{name: "file.txt", contents: []byte("old"), etag: `"tag1"`, mtime: mtime}
	s := NewServer("/", newMemColl("root", file))

	req := httptest.NewRequest("PUT", "/file.txt", strings.NewReader("new"))
	req.Header.Set("If-None-Match", `"nomatch"`)
	// The resource changed after this date, so the write must fail with 412.
	req.Header.Set("If-Unmodified-Since", mtime.Add(-24*time.Hour).Format(http.TimeFormat))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 (If-Unmodified-Since must be evaluated even when If-None-Match is present)", rec.Code)
	}
	if string(file.contents) != "old" {
		t.Errorf("PUT body written despite failed precondition; contents = %q", file.contents)
	}
}

// A compressing reverse proxy (Caddy `encode`, nginx `gzip`) rewrites the
// strong ETag it forwards on a GET into a weak one; the browser app then replays
// that weak tag as If-Match on its PUT. That conditional write must still match
// the origin's strong ETag — otherwise every save behind such a proxy 412s.
func TestPreconditionWeakIfMatchStillMatches(t *testing.T) {
	file := &memFile{name: "file.txt", contents: []byte("old"), etag: `"tag1"`}
	s := NewServer("/", newMemColl("root", file))

	req := httptest.NewRequest("PUT", "/file.txt", strings.NewReader("new"))
	req.Header.Set("If-Match", `W/"tag1"`) // weakened in transit by a gzip proxy
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code == http.StatusPreconditionFailed {
		t.Fatalf("status = 412; a weak If-Match must match the strong ETag (proxy-gzip case)")
	}
	if string(file.contents) != "new" {
		t.Errorf("PUT body not written; contents = %q, want %q", file.contents, "new")
	}
}

// A weak If-Match that names a *different* tag must still fail: the W/ tolerance
// only strips the weak marker, it does not make mismatched tags match.
func TestPreconditionWeakIfMatchWrongTagStillFails(t *testing.T) {
	file := &memFile{name: "file.txt", contents: []byte("old"), etag: `"tag1"`}
	s := NewServer("/", newMemColl("root", file))

	req := httptest.NewRequest("PUT", "/file.txt", strings.NewReader("new"))
	req.Header.Set("If-Match", `W/"other"`)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 for a non-matching weak If-Match", rec.Code)
	}
	if string(file.contents) != "old" {
		t.Errorf("PUT body written despite mismatched If-Match; contents = %q", file.contents)
	}
}

func TestNormalizeETag(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"abc"`, `"abc"`},
		{`W/"abc"`, `"abc"`},
		{`w/"abc"`, `"abc"`},
		{` W/"abc" `, `"abc"`},
		{`\"abc\"`, `"abc"`}, // Evolution escaping
	} {
		if got := normalizeETag(tc.in); got != tc.want {
			t.Errorf("normalizeETag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Bug: the same early return also skips the If: header, so a failing tagged
// condition (RFC 4918 §10.4) is ignored when If-None-Match is present.
func TestPreconditionIfNoneMatchDoesNotSuppressIfHeader(t *testing.T) {
	file := &memFile{name: "file.txt", contents: []byte("old"), etag: `"tag1"`}
	s := NewServer("/", newMemColl("root", file))

	req := httptest.NewRequest("PUT", "/file.txt", strings.NewReader("new"))
	req.Header.Set("If-None-Match", `"nomatch"`)
	// The If condition requires an ETag the resource doesn't have → 412.
	req.Header.Set("If", `(["wrong"])`)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 (the If header must be evaluated even when If-None-Match is present)", rec.Code)
	}
	if string(file.contents) != "old" {
		t.Errorf("PUT body written despite failed If condition; contents = %q", file.contents)
	}
}
