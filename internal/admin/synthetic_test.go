package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// httptest.NewRequest panics on a target it cannot parse as a request line
// (a path with a space); the simulator's synthetic requests must not
// (audit 6.12).
func TestSyntheticRequestToleratesOddPaths(t *testing.T) {
	for _, path := range []string{"/calendars/alice/my cal/", "/calendars/alice/ü/", "/principals/a%2Fb", "/x?y#z"} {
		req := syntheticRequest("PROPFIND", path, "")
		if req.URL.Path != path {
			t.Errorf("%q: URL.Path = %q", path, req.URL.Path)
		}
		if req.Method != "PROPFIND" {
			t.Errorf("%q: method = %q", path, req.Method)
		}
	}
	// The whole simulate route survives such a path.
	h := testApp().Handler()
	req := httptest.NewRequest("GET", "/api/simulate?principal=principals/alice&path=/calendars/alice/my%20cal/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusInternalServerError {
		t.Errorf("simulate with a space in the path = %d:\n%s", w.Code, w.Body.String())
	}
}
