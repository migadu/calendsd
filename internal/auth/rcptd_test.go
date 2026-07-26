package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
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
	b.Provision = func(principal, email string) error {
		provPrincipal, provEmail = principal, email
		return nil
	}

	principal, ok, reason := b.Check(basicReq(t, "User@Example.com", "s3cret"))
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

	_, ok, reason := b.Check(basicReq(t, "user@example.com", "wrong"))
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

	_, ok, reason := b.Check(basicReq(t, "ghost@example.com", "whatever"))
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

	_, ok, reason := b.Check(basicReq(t, "user@example.com", "s3cret"))
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

	_, ok, _ := b.Check(basicReq(t, "user@example.com", "s3cret"))
	if ok {
		t.Fatal("Check succeeded with a wrong bearer token")
	}
}

func TestRcptdNoBasicHeader(t *testing.T) {
	f := &fakeRcptd{}
	b, _ := newBackend(t, f, 0)
	_, ok, _ := b.Check(basicReq(t, "", ""))
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
		if _, ok, reason := b.Check(basicReq(t, "user@example.com", "s3cret")); !ok {
			t.Fatalf("Check %d failed: %s", i, reason)
		}
	}
	if got := atomic.LoadInt32(&f.hits); got != 1 {
		t.Errorf("rcptd hit %d times across 3 identical logins; want 1 (cache)", got)
	}

	// A wrong password must not be served from the positive cache.
	if _, ok, _ := b.Check(basicReq(t, "user@example.com", "nope")); ok {
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
	b.Provision = func(principal, email string) error {
		return errProvision
	}
	if _, ok, _ := b.Check(basicReq(t, "user@example.com", "s3cret")); ok {
		t.Fatal("Check succeeded despite provisioning failure; must fail closed")
	}
}

var errProvision = &provisionError{}

type provisionError struct{}

func (*provisionError) Error() string { return "boom" }
