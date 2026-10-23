package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Audit 5.8: the panel had no failure budget and compared unknown users
// against a cost-10 dummy while real entries are cost-5, a timing tell.
func TestBasicAuthThrottles(t *testing.T) {
	creds := map[string]string{"root": bcryptHash(t, "s3cret")}
	byAddress, byUser := adminThrottle()
	frozen := time.Unix(1_700_000_000, 0) // no wall-clock refill during the test
	byAddress.Now = func() time.Time { return frozen }
	byUser.Now = func() time.Time { return frozen }
	h := basicAuthWith(creds, byAddress, byUser, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	try := func(user, pass, remote string) (int, string) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		req.SetBasicAuth(user, pass)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Header().Get("Retry-After")
	}
	for i := 0; i < 20; i++ {
		if code, _ := try("root", "wrong", "127.0.0.1:5"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	code, ra := try("root", "s3cret", "127.0.0.1:6")
	if code != http.StatusTooManyRequests || ra == "" {
		t.Fatalf("21st attempt: %d (Retry-After %q), want 429 with Retry-After", code, ra)
	}
	// Loopback callers (the panel's normal case) are keyed per username
	// only, so one operator's failures do not lock out another.
	if code, _ := try("other", "x", "127.0.0.1:7"); code != http.StatusUnauthorized {
		t.Errorf("loopback, other user: %d, want 401 (no shared address bucket)", code)
	}
	// A non-loopback address does get an address bucket.
	for i := 0; i < 20; i++ {
		try("u"+string(rune('a'+i)), "wrong", "198.51.100.9:1")
	}
	if code, _ := try("fresh", "x", "198.51.100.9:2"); code != http.StatusTooManyRequests {
		t.Errorf("remote address after 20 failures: %d, want 429", code)
	}
}

func TestDecoyHashIsARealEntry(t *testing.T) {
	creds := map[string]string{"zed": "$2y$05$zzz", "amy": "$2y$05$aaa"}
	if got := decoyHash(creds); got != "$2y$05$aaa" {
		t.Errorf("decoyHash = %q, want the lowest user's real hash", got)
	}
}
