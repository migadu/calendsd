package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"calendsd/internal/dav"
)

// rejecting is a Basic backend that accepts one password.
type rejecting struct{ pass string }

func (b rejecting) Check(r *http.Request) (string, error) {
	u, p, ok := r.BasicAuth()
	if !ok {
		return "", errors.New("no credentials")
	}
	if p != b.pass {
		return "", ErrBadCredentials
	}
	return "principals/" + u, nil
}
func (rejecting) Challenge(h http.Header, _ *http.Request) {
	h.Add("WWW-Authenticate", `Basic realm="t"`)
}

func attempt(t *testing.T, p *Plugin, user, pass, remote, xff string) (int, http.Header) {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if user != "" {
		r.SetBasicAuth(user, pass)
	}
	w := httptest.NewRecorder()
	proceed, err := p.BeforeMethod(w, r)
	if proceed {
		return 200, w.Header()
	}
	de := dav.AsError(err)
	hdr := w.Header()
	if de.RetryAfter > 0 {
		hdr.Set("Retry-After", de.RetryAfter.String())
	}
	return de.Status, hdr
}

func newLimitedPlugin() (*Plugin, *Limiter) {
	l := NewLimiter(rejecting{pass: "right"})
	// A pinned clock: the buckets refill with wall time, and a slow run
	// must not hand the 21st attempt a refilled token.
	frozen := time.Unix(1_700_000_000, 0)
	l.ByAddress.Now = func() time.Time { return frozen }
	l.ByUser.Now = func() time.Time { return frozen }
	p := NewPlugin(l)
	p.AutoRequireLogin = true
	return p, l
}

func TestLimiterThrottlesPerAddress(t *testing.T) {
	p, _ := newLimitedPlugin()
	for i := 0; i < 20; i++ {
		if code, _ := attempt(t, p, "alice", "wrong", "198.51.100.7:1", ""); code != 401 {
			t.Fatalf("attempt %d: %d, want 401", i, code)
		}
	}
	code, hdr := attempt(t, p, "bob", "wrong", "198.51.100.7:2", "")
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" {
		t.Fatalf("21st failure from one address: %d (Retry-After %q), want 429 with Retry-After", code, hdr.Get("Retry-After"))
	}
	if hdr.Get("WWW-Authenticate") != "" {
		t.Error("a 429 must not challenge")
	}
	// Even the right password is refused while throttled (the backend is
	// never consulted), and another address is unaffected.
	if code, _ := attempt(t, p, "alice", "right", "198.51.100.7:3", ""); code != http.StatusTooManyRequests {
		t.Errorf("throttled address with the right password: %d, want 429", code)
	}
	if code, _ := attempt(t, p, "carol", "right", "203.0.113.9:1", ""); code != 200 {
		t.Errorf("other address: %d, want 200", code)
	}
}

func TestLimiterThrottlesPerUsernameAcrossAddresses(t *testing.T) {
	p, _ := newLimitedPlugin()
	for i := 0; i < 20; i++ {
		remote := "203.0.113." + string(rune('0'+i%10)) + ":1"
		if i >= 10 {
			remote = "198.51.100." + string(rune('0'+i%10)) + ":1"
		}
		if code, _ := attempt(t, p, "Alice", "wrong", remote, ""); code != 401 {
			t.Fatalf("attempt %d: %d, want 401", i, code)
		}
	}
	if code, _ := attempt(t, p, "alice", "right", "192.0.2.77:1", ""); code != http.StatusTooManyRequests {
		t.Errorf("21st failure for one username from a fresh address: %d, want 429", code)
	}
	if code, _ := attempt(t, p, "dave", "right", "192.0.2.77:1", ""); code != 200 {
		t.Errorf("other username from the same address: %d, want 200", code)
	}
}

func TestLimiterSuccessDoesNotRefill(t *testing.T) {
	p, _ := newLimitedPlugin()
	for i := 0; i < 19; i++ {
		attempt(t, p, "alice", "wrong", "198.51.100.7:1", "")
	}
	for i := 0; i < 5; i++ {
		if code, _ := attempt(t, p, "eve", "right", "198.51.100.7:1", ""); code != 200 {
			t.Fatalf("success %d: %d", i, code)
		}
	}
	attempt(t, p, "alice", "wrong", "198.51.100.7:1", "")
	if code, _ := attempt(t, p, "alice", "wrong", "198.51.100.7:1", ""); code != http.StatusTooManyRequests {
		t.Errorf("successes refilled the failure budget: %d, want 429", code)
	}
}

func TestLimiterLoopbackKeys(t *testing.T) {
	p, _ := newLimitedPlugin()
	// Loopback without X-Forwarded-For (the contacts API): no address key,
	// so twenty different users' failures do not lock a twenty-first out.
	for i := 0; i < 25; i++ {
		attempt(t, p, "user"+string(rune('a'+i)), "wrong", "127.0.0.1:1", "")
	}
	if code, _ := attempt(t, p, "fresh", "right", "127.0.0.1:1", ""); code != 200 {
		t.Errorf("loopback callers were bucketed together: %d, want 200", code)
	}
	// Loopback with X-Forwarded-For (behind calclient): the LAST forwarded
	// entry is the key — the proxy appends the address it saw, so a
	// client-supplied first entry must not pick the bucket.
	for i := 0; i < 20; i++ {
		spoofed := "10.9.9." + string(rune('0'+i%10))
		attempt(t, p, "user"+string(rune('a'+i)), "wrong", "127.0.0.1:1", spoofed+", 198.51.100.50")
	}
	if code, _ := attempt(t, p, "fresh2", "right", "127.0.0.1:1", "198.51.100.50"); code != http.StatusTooManyRequests {
		t.Errorf("forwarded address not throttled despite rotating spoofed first entries: %d, want 429", code)
	}
	if code, _ := attempt(t, p, "fresh3", "right", "127.0.0.1:1", "198.51.100.51"); code != 200 {
		t.Errorf("other forwarded address affected: %d, want 200", code)
	}
}

// Server-side faults are not the client's failures: a wrong password counts,
// an rcptd outage or a provisioning error does not.
func TestLimiterCountsOnlyBadCredentials(t *testing.T) {
	flaky := &errBackend{err: ErrUnavailable}
	l := NewLimiter(flaky)
	frozen := time.Unix(1_700_000_000, 0)
	l.ByAddress.Now = func() time.Time { return frozen }
	l.ByUser.Now = func() time.Time { return frozen }
	p := NewPlugin(l)
	p.AutoRequireLogin = true
	for i := 0; i < 30; i++ {
		attempt(t, p, "alice", "x", "198.51.100.7:1", "")
	}
	flaky.err = &serverError{msg: "provisioning failed", err: errors.New("db down")}
	for i := 0; i < 30; i++ {
		attempt(t, p, "alice", "x", "198.51.100.7:1", "")
	}
	if w := l.ByUser.RetryAfter("alice"); w != 0 {
		t.Errorf("server faults counted against the user: wait %v", w)
	}
	flaky.err = ErrBadCredentials
	for i := 0; i < 20; i++ {
		attempt(t, p, "alice", "x", "198.51.100.7:1", "")
	}
	if w := l.ByUser.RetryAfter("alice"); w == 0 {
		t.Error("bad credentials not counted")
	}
}

type errBackend struct{ err error }

func (b *errBackend) Check(r *http.Request) (string, error) {
	if _, _, ok := r.BasicAuth(); !ok {
		return "", errors.New("no credentials")
	}
	return "", b.err
}
func (*errBackend) Challenge(http.Header, *http.Request) {}

func TestThrottleRefillsAndParses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	th := &Throttle{Rate: 1.0 / 6, Burst: 2, Now: func() time.Time { return now }}
	th.Fail("k")
	th.Fail("k")
	if w := th.RetryAfter("k"); w != 6*time.Second {
		t.Errorf("RetryAfter after burst = %v, want 6s", w)
	}
	now = now.Add(6 * time.Second)
	if w := th.RetryAfter("k"); w != 0 {
		t.Errorf("RetryAfter after refill = %v, want 0", w)
	}
	if ra, ok := Throttled(&ThrottledError{RetryAfter: 42 * time.Second}); !ok || ra != 42*time.Second {
		t.Errorf("Throttled round trip = %v %v", ra, ok)
	}
	if _, ok := Throttled(ErrBadCredentials); ok {
		t.Error("ordinary rejection parsed as throttled")
	}
	// Credential-less probes never count.
	p, l := newLimitedPlugin()
	for i := 0; i < 30; i++ {
		attempt(t, p, "", "", "198.51.100.7:1", "")
	}
	if w := l.ByAddress.RetryAfter("198.51.100.7"); w != 0 {
		t.Errorf("probes without credentials counted as failures: wait %v", w)
	}
}
