package contactsapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"calendsd/internal/auth"
	"calendsd/internal/contactsapi"
)

// passwordAuth rejects with the backends' real bad-credentials error, the
// only rejection the limiter counts.
type passwordAuth struct{}

func (passwordAuth) Check(r *http.Request) (string, error) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return "", errors.New("no credentials")
	}
	if pass != "pw" {
		return "", auth.ErrBadCredentials
	}
	return "principals/" + user, nil
}
func (passwordAuth) Challenge(h http.Header, _ *http.Request) {
	h.Set("WWW-Authenticate", `Basic realm="t"`)
}

// The contacts API authenticates through the shared backend, which the
// limiter now wraps; a throttled rejection must surface as 429 with
// Retry-After rather than a 401 that invites the next guess (audit 5.8).
func TestThrottledLoginIs429(t *testing.T) {
	db := openScratchDB(t, "calserver_capi_throttle")
	limited := auth.NewLimiter(passwordAuth{})
	frozen := time.Unix(1_700_000_000, 0) // no wall-clock refill during the test
	limited.ByAddress.Now = func() time.Time { return frozen }
	limited.ByUser.Now = func() time.Time { return frozen }
	h := contactsapi.New(db, limited, func(context.Context, string) bool { return false }, contactsapi.Options{})

	try := func(pass string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/contacts", nil)
		req.RemoteAddr = "127.0.0.1:4242" // loopback, no X-Forwarded-For: username key only
		req.SetBasicAuth("alice@example.org", pass)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < 20; i++ {
		if w := try("nope"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := try("pw")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("after 20 failures: %d (Retry-After %q), want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	// Another user on the same loopback listener is unaffected.
	req := httptest.NewRequest("GET", "/api/v1/contacts", nil)
	req.RemoteAddr = "127.0.0.1:4243"
	req.SetBasicAuth("bob@example.org", "pw")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Error("another user's login throttled by the first user's failures")
	}
}
