package dav

// Audit 4.3: If-Match was checked, then the PUT went through unconditionally;
// a writer landing in between was silently overwritten. A node that can do a
// conditional write now gets the matched tag, and a mismatch is a 412.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// racyFile reports ETag "a" but its conditional write says the stored tag
// has moved on, which is what a concurrent writer looks like to storage.
type racyFile struct {
	*memFile
	putIfMatchCalls int
	mismatch        bool
}

func (f *racyFile) PutIfMatch(_ context.Context, data []byte, expected string) (string, error) {
	f.putIfMatchCalls++
	if f.mismatch {
		return "", ErrETagMismatch
	}
	f.contents = data
	f.etag = `"b"`
	return f.etag, nil
}

func TestPutWithIfMatchIsConditional(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mismatch bool
		want     int
	}{
		{"concurrent writer → 412", true, http.StatusPreconditionFailed},
		{"no writer → 204", false, http.StatusNoContent},
	} {
		f := &racyFile{memFile: &memFile{name: "r.txt", contents: []byte("old"), etag: `"a"`}, mismatch: tc.mismatch}
		root := newMemColl("root", f)
		s := NewServer("/", root)

		req := httptest.NewRequest("PUT", "/r.txt", strings.NewReader("new"))
		req.Header.Set("If-Match", `"a"`)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)

		if rec.Code != tc.want {
			t.Errorf("%s: PUT = %d, want %d: %s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		if f.putIfMatchCalls != 1 {
			t.Errorf("%s: PutIfMatch called %d times, want 1 (the conditional path must be used)", tc.name, f.putIfMatchCalls)
		}
		if tc.mismatch && string(f.contents) != "old" {
			t.Errorf("%s: content overwritten despite the mismatch", tc.name)
		}
	}

	// Without If-Match the plain Put path is used.
	f := &racyFile{memFile: &memFile{name: "r.txt", contents: []byte("old"), etag: `"a"`}, mismatch: true}
	s := NewServer("/", newMemColl("root", f))
	req := httptest.NewRequest("PUT", "/r.txt", strings.NewReader("new"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || f.putIfMatchCalls != 0 {
		t.Errorf("unconditional PUT = %d (PutIfMatch calls %d), want 204 via Put", rec.Code, f.putIfMatchCalls)
	}
}
