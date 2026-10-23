package admin

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The panel may never be framed (audit 5.7): every response carries
// frame-ancestors 'none' and X-Frame-Options: DENY, whatever its status.
func TestSecurityHeadersForbidFraming(t *testing.T) {
	h := testApp().Handler()
	for _, path := range []string{"/", "/principal?username=alice", "/api/check-credentials", "/nope"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		hdr := rec.Header()
		if got := hdr.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", path, got)
		}
		if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP lacks frame-ancestors 'none': %q", path, csp)
		}
	}
}
