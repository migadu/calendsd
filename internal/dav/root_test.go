package dav

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The root refuses writes itself, independent of any ACL plugin.
func TestRootIsReadOnly(t *testing.T) {
	s := NewServer("/", NewRoot(newMemColl("calendars")))
	for _, tc := range []struct{ method, path string }{
		{"PUT", "/junk.txt"},
		{"MKCOL", "/junk/"},
		{"DELETE", "/"},
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("PROPFIND", "/calendars/", nil))
	if rec.Code != http.StatusMultiStatus {
		t.Errorf("PROPFIND /calendars/ = %d, want 207", rec.Code)
	}
}
