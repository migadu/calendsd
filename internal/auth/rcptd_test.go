package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/crypto/bcrypt"

	"github.com/migadu/calendsd/internal/metrics"
)

// fakeRcptd stands in for the rcptd /auth endpoint. It serves a fixed
// user→hashes map, enforces a bearer token, and counts requests so tests can
// assert the positive-auth cache elides round-trips.
type fakeRcptd struct {
	token  string
	users  map[string][]string // email → password_hashes
	hits   int32
	status int // when non-zero, force this status for a known user (e.g. 503)
}

func (f *fakeRcptd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.hits, 1)
	if r.URL.Path != "/auth" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	email := r.URL.Query().Get("email")
	hashes, ok := f.users[email]
	if !ok {
		http.Error(w, "user unknown", http.StatusNotFound)
		return
	}
	if f.status != 0 {
		http.Error(w, "forced", f.status)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"password_hashes": hashes,
		"allowed_from":    []string{email},
	})
}

func bcryptHashT(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

// basicReq builds a GET carrying Basic credentials.
func basicReq(t *testing.T, user, pass string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if user != "" || pass != "" {
		r.SetBasicAuth(user, pass)
	}
	return r
}

func newBackend(t *testing.T, f *fakeRcptd, ttl time.Duration) (*RcptdBackend, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	b := NewRcptdBackend("Realm", ts.URL, f.token, ttl)
	return b, ts
}

func TestRcptdCheckSuccess(t *testing.T) {
	f := &fakeRcptd{token: "t0k", users: map[string][]string{
		"user@example.com": {bcryptHashT(t, "s3cret")},
	}}
	b, _ := newBackend(t, f, 0)

	var provPrincipal, provEmail string
	b.Provision = func(_ context.Context, principal, email string) error {
		provPrincipal, provEmail = principal, email
		return nil
	}

	principal, ok, reason := check(b, basicReq(t, "User@Example.com", "s3cret"))
	if !ok {
		t.Fatalf("Check failed: %s", reason)
	}
	if principal != "principals/user@example.com" {
		t.Errorf("principal = %q, want principals/user@example.com (lowercased)", principal)
	}
	if provPrincipal != "principals/user@example.com" || provEmail != "user@example.com" {
		t.Errorf("Provision(%q, %q); want lowercased principal + email", provPrincipal, provEmail)
	}
}

func TestRcptdCheckWrongPassword(t *testing.T) {
	f := &fakeRcptd{users: map[string][]string{
		"user@example.com": {bcryptHashT(t, "s3cret")},
	}}
	b, _ := newBackend(t, f, 0)

	_, ok, reason := check(b, basicReq(t, "user@example.com", "wrong"))
	if ok {
		t.Fatal("Check succeeded with wrong password")
	}
	if reason != "Username or password was incorrect" {
		t.Errorf("reason = %q, want the generic message (no account enumeration)", reason)
	}
}

func TestRcptdCheckUnknownUser(t *testing.T) {
	f := &fakeRcptd{users: map[string][]string{}}
	b, _ := newBackend(t, f, 0)

	_, ok, reason := check(b, basicReq(t, "ghost@example.com", "whatever"))
	if ok {
		t.Fatal("Check succeeded for unknown user")
	}
	// Same message as wrong-password: a 404 must not be distinguishable.
	if reason != "Username or password was incorrect" {
		t.Errorf("reason = %q, want the generic message", reason)
	}
}

func TestRcptdCheckServiceUnavailable(t *testing.T) {
	f := &fakeRcptd{
		users:  map[string][]string{"user@example.com": {bcryptHashT(t, "s3cret")}},
		status: http.StatusServiceUnavailable,
	}
	b, _ := newBackend(t, f, 0)

	_, ok, reason := check(b, basicReq(t, "user@example.com", "s3cret"))
	if ok {
		t.Fatal("Check succeeded despite a 503 from rcptd")
	}
	if reason == "Username or password was incorrect" {
		t.Error("a transient 503 must not read as a definitive auth failure")
	}
}

func TestRcptdCheckBadServerToken(t *testing.T) {
	f := &fakeRcptd{token: "correct", users: map[string][]string{
		"user@example.com": {bcryptHashT(t, "s3cret")},
	}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	b := NewRcptdBackend("Realm", ts.URL, "WRONG-token", 0) // misconfigured

	_, ok, _ := check(b, basicReq(t, "user@example.com", "s3cret"))
	if ok {
		t.Fatal("Check succeeded with a wrong bearer token")
	}
}

func TestRcptdNoBasicHeader(t *testing.T) {
	f := &fakeRcptd{}
	b, _ := newBackend(t, f, 0)
	_, ok, _ := check(b, basicReq(t, "", ""))
	if ok {
		t.Fatal("Check succeeded without an Authorization header")
	}
	if got := atomic.LoadInt32(&f.hits); got != 0 {
		t.Errorf("hit rcptd %d times without credentials; want 0", got)
	}
}

func TestRcptdPositiveCache(t *testing.T) {
	f := &fakeRcptd{users: map[string][]string{
		"user@example.com": {bcryptHashT(t, "s3cret")},
	}}
	b, _ := newBackend(t, f, time.Minute)

	for i := 0; i < 3; i++ {
		if _, ok, reason := check(b, basicReq(t, "user@example.com", "s3cret")); !ok {
			t.Fatalf("Check %d failed: %s", i, reason)
		}
	}
	if got := atomic.LoadInt32(&f.hits); got != 1 {
		t.Errorf("rcptd hit %d times across 3 identical logins; want 1 (cache)", got)
	}

	// A wrong password must not be served from the positive cache.
	if _, ok, _ := check(b, basicReq(t, "user@example.com", "nope")); ok {
		t.Fatal("wrong password accepted from cache")
	}
	if got := atomic.LoadInt32(&f.hits); got != 2 {
		t.Errorf("rcptd hit %d times; want 2 (wrong password re-checked)", got)
	}
}

func TestRcptdProvisionErrorFailsClosed(t *testing.T) {
	f := &fakeRcptd{users: map[string][]string{
		"user@example.com": {bcryptHashT(t, "s3cret")},
	}}
	b, _ := newBackend(t, f, 0)
	b.Provision = func(_ context.Context, principal, email string) error {
		return errProvision
	}
	if _, ok, _ := check(b, basicReq(t, "user@example.com", "s3cret")); ok {
		t.Fatal("Check succeeded despite provisioning failure; must fail closed")
	}

	// The database error is logged, but the 401 body never carries it.
	var logs strings.Builder
	p := NewPlugin(b)
	p.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	_, err := p.BeforeMethod(httptest.NewRecorder(), basicReq(t, "user@example.com", "s3cret"))
	if err == nil {
		t.Fatal("plugin let the request through")
	}
	if msg := err.Error(); strings.Contains(msg, "boom") || !strings.Contains(msg, "provisioning failed") {
		t.Errorf("401 message = %q, want the generic provisioning failure without the database error", msg)
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Errorf("log lacks the provisioning error:\n%s", logs.String())
	}
}

var errProvision = &provisionError{}

type provisionError struct{}

func (*provisionError) Error() string { return "boom" }

// TestRcptdChallenge: the 401 challenge advertises Basic with the realm.
func TestRcptdChallenge(t *testing.T) {
	b := NewRcptdBackend("CalDAV", "http://unused", "", 0)
	h := http.Header{}
	b.Challenge(h, httptest.NewRequest("GET", "/", nil))
	if got, want := h.Get("WWW-Authenticate"), `Basic realm="CalDAV", charset="UTF-8"`; got != want {
		t.Errorf("challenge = %q, want %q", got, want)
	}
}

// TestRcptdUnreachable: a transport failure (rcptd down) reads as "service
// unreachable", never as wrong credentials.
func TestRcptdUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close() // dial now fails
	b := NewRcptdBackend("Realm", ts.URL, "", 0)

	_, ok, reason := check(b, basicReq(t, "user@example.com", "pw"))
	if ok {
		t.Fatal("Check succeeded with rcptd unreachable")
	}
	if reason != "authentication service is unreachable" {
		t.Errorf("reason = %q, want the unreachable message", reason)
	}
	if _, err := b.Check(basicReq(t, "user@example.com", "pw")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Check = %v, want ErrUnavailable", err)
	}
}

// TestRcptdBadBaseURL: a base URL that cannot form a request (operator typo)
// fails the lookup, surfacing as service-unreachable rather than panicking or
// leaking a Go error to the client.
func TestRcptdBadBaseURL(t *testing.T) {
	b := NewRcptdBackend("Realm", "http://bad host", "", 0)
	_, ok, reason := check(b, basicReq(t, "user@example.com", "pw"))
	if ok {
		t.Fatal("Check succeeded with an unusable base URL")
	}
	if reason != "authentication service is unreachable" {
		t.Errorf("reason = %q, want the unreachable message", reason)
	}
}

// TestRcptdUnexpectedStatus: a status verify() doesn't distinguish (500 here)
// is a typed "error" outcome naming the code, and the requests metric
// collapses it to code="other" so a remote server can't mint label series.
func TestRcptdUnexpectedStatus(t *testing.T) {
	f := &fakeRcptd{
		users:  map[string][]string{"user@example.com": {bcryptHashT(t, "s3cret")}},
		status: http.StatusInternalServerError,
	}
	b, _ := newBackend(t, f, 0)
	set := metrics.New("test")
	b.Metrics = set

	_, ok, reason := check(b, basicReq(t, "user@example.com", "s3cret"))
	if ok {
		t.Fatal("Check succeeded despite a 500 from rcptd")
	}
	if reason != "authentication service returned status 500" {
		t.Errorf("reason = %q, want the status-500 message", reason)
	}
	want := `
# HELP calserver_rcptd_requests_total HTTP lookups against rcptd /auth by status code (or "error" for transport failures).
# TYPE calserver_rcptd_requests_total counter
calserver_rcptd_requests_total{code="other"} 1
`
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(want),
		"calserver_rcptd_requests_total"); err != nil {
		t.Error(err)
	}
}

// TestRcptdMalformedBody: a 200 whose body isn't the /auth JSON (a proxy
// error page) fails the login and counts as code="error", not code="200",
// so the rcptd health alert sees the outage.
func TestRcptdMalformedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>this is not JSON</html>"))
	}))
	t.Cleanup(ts.Close)
	b := NewRcptdBackend("Realm", ts.URL, "", 0)
	set := metrics.New("test")
	b.Metrics = set

	if _, ok, _ := check(b, basicReq(t, "user@example.com", "pw")); ok {
		t.Fatal("Check succeeded on an undecodable /auth body")
	}
	want := `
# HELP calserver_rcptd_requests_total HTTP lookups against rcptd /auth by status code (or "error" for transport failures).
# TYPE calserver_rcptd_requests_total counter
calserver_rcptd_requests_total{code="error"} 1
`
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(want),
		"calserver_rcptd_requests_total"); err != nil {
		t.Error(err)
	}
}

// TestRcptdCustomHTTPClient: an injected HTTP client is used for lookups in
// place of the default 5s-timeout client.
func TestRcptdCustomHTTPClient(t *testing.T) {
	f := &fakeRcptd{users: map[string][]string{
		"user@example.com": {bcryptHashT(t, "s3cret")},
	}}
	b, _ := newBackend(t, f, 0)
	b.HTTP = &http.Client{Timeout: time.Second}

	if _, ok, reason := check(b, basicReq(t, "user@example.com", "s3cret")); !ok {
		t.Fatalf("Check with injected client failed: %s", reason)
	}
}

// TestRcptdUnusableHashLogged: stored hashes in an unsupported scheme are
// warned about — such a user can never authenticate, and silence would make
// that look like a plain wrong password.
func TestRcptdUnusableHashLogged(t *testing.T) {
	f := &fakeRcptd{users: map[string][]string{
		"user@example.com": {"{NOPE}unsupported-scheme"},
	}}
	b, _ := newBackend(t, f, 0)
	cap := &captureHandler{}
	b.Logger = slog.New(cap)

	if _, ok, _ := check(b, basicReq(t, "user@example.com", "whatever")); ok {
		t.Fatal("Check succeeded against an unusable hash")
	}
	rec, attrs := cap.only(t)
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", rec.Level)
	}
	if attrs["user"] != "user@example.com" || attrs["unsupported"] != "1" {
		t.Errorf("attrs = %v, want user and unsupported=1", attrs)
	}
}

// TestRcptdCacheStorePurgesExpired: storing a fresh credential drops expired
// entries, so a churn of distinct logins can't grow the map without bound.
func TestRcptdCacheStorePurgesExpired(t *testing.T) {
	f := &fakeRcptd{users: map[string][]string{
		"user@example.com": {bcryptHashT(t, "s3cret")},
	}}
	b, _ := newBackend(t, f, time.Minute)

	// Seed an already-expired entry for another account.
	b.mu.Lock()
	b.cache["old@example.com"] = cacheEntry{expiry: time.Now().Add(-time.Second)}
	b.mu.Unlock()

	if _, ok, reason := check(b, basicReq(t, "user@example.com", "s3cret")); !ok {
		t.Fatalf("Check failed: %s", reason)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, still := b.cache["old@example.com"]; still {
		t.Error("expired cache entry survived a store")
	}
	if _, cached := b.cache["user@example.com"]; !cached {
		t.Error("fresh login was not cached")
	}
}
