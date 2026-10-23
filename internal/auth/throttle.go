package auth

// Login throttling (audit 5.8). Every DAV Basic attempt costs an rcptd round
// trip plus a bcrypt verification, and the contacts API authenticates through
// the same backend; nothing bounded how fast a client could try passwords.
//
// Throttle is a keyed token bucket of *failures*: a key may fail Burst times
// at once and then Rate times per second; a successful login refills nothing
// (success must not help an attacker pace a guess). Limiter wraps the auth
// backends with two keys per attempt: the client address, and the attempted
// username (which also stops distributed guessing of one account). On a
// loopback listener without X-Forwarded-For — the contacts API, where every
// webmail user arrives as 127.0.0.1 — only the username key applies, so one
// user's failures cannot lock the whole address book.

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Throttle is a bounded map of failure buckets.
type Throttle struct {
	// Rate is the refill in failures per second; Burst the bucket size. A
	// zero Burst disables the throttle.
	Rate, Burst float64
	// MaxEntries bounds the map; idle (full) buckets are dropped when it is
	// reached. Zero means 10000.
	MaxEntries int
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens  float64
	updated time.Time
}

func (t *Throttle) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Throttle) refilled(key string, now time.Time) *bucket {
	b, ok := t.buckets[key]
	if !ok {
		return nil
	}
	b.tokens = math.Min(t.Burst, b.tokens+now.Sub(b.updated).Seconds()*t.Rate)
	b.updated = now
	return b
}

// RetryAfter reports how long key must wait before another attempt is
// allowed; zero means it may try now.
func (t *Throttle) RetryAfter(key string) time.Duration {
	if t == nil || t.Burst <= 0 || key == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.refilled(key, t.now())
	if b == nil || b.tokens >= 1 {
		return 0
	}
	if t.Rate <= 0 {
		return time.Hour
	}
	secs := (1 - b.tokens) / t.Rate
	return time.Duration(math.Ceil(secs)) * time.Second
}

// Fail records a failed attempt for key.
func (t *Throttle) Fail(key string) {
	if t == nil || t.Burst <= 0 || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	b := t.refilled(key, now)
	if b == nil {
		t.prune(now)
		b = &bucket{tokens: t.Burst, updated: now}
		t.buckets[key] = b
	}
	b.tokens = math.Max(0, b.tokens-1)
}

// prune drops idle buckets once the map is full; called with mu held.
func (t *Throttle) prune(now time.Time) {
	if t.buckets == nil {
		t.buckets = map[string]*bucket{}
	}
	limit := t.MaxEntries
	if limit <= 0 {
		limit = 10000
	}
	if len(t.buckets) < limit {
		return
	}
	for k := range t.buckets {
		if b := t.refilled(k, now); b.tokens >= t.Burst {
			delete(t.buckets, k)
		}
	}
	if len(t.buckets) >= limit {
		// Still full of live buckets: evict arbitrary ones rather than grow
		// without bound. An attacker who fills this map is already throttled.
		for k := range t.buckets {
			delete(t.buckets, k)
			if len(t.buckets) < limit/2 {
				break
			}
		}
	}
}

// ThrottledError is the Check failure for a throttled attempt. It carries
// the wait so callers answer 429 with Retry-After; its text is safe to show.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("too many failed login attempts; retry after %d seconds", int(math.Ceil(e.RetryAfter.Seconds())))
}

// Throttled reports whether err is a throttle, and the wait it carries.
func Throttled(err error) (retryAfter time.Duration, ok bool) {
	var te *ThrottledError
	if errors.As(err, &te) {
		return te.RetryAfter, true
	}
	return 0, false
}

// Limiter is a Backend that throttles failed attempts across the backends
// it wraps. It tries them in order like Plugin does, so it stands in for the
// whole set: the DAV plugin and the contacts API both authenticate through
// it, which is what makes the limit cover every login path.
type Limiter struct {
	Backends []Backend
	// ByAddress throttles per client address; ByUser per attempted username.
	// Either nil disables that key.
	ByAddress, ByUser *Throttle
}

// NewLimiter wraps backends with the default budgets: 10 failures per minute
// with a burst of 20 per address, and 20 failures per 5 minutes per username.
func NewLimiter(backends ...Backend) *Limiter {
	return &Limiter{
		Backends:  backends,
		ByAddress: &Throttle{Rate: 10.0 / 60, Burst: 20},
		ByUser:    &Throttle{Rate: 20.0 / 300, Burst: 20},
	}
}

// ClientAddress is the throttling key for the client: the X-Forwarded-For
// address when the request arrived through the loopback reverse proxy
// (calclient sets it), the connection's address otherwise. A loopback
// request without X-Forwarded-For has no usable address key: on the
// contacts API every caller is loopback, and one bucket for all of them
// would let any user's failures lock everyone out.
//
// Of a multi-entry X-Forwarded-For the LAST entry is used: a reverse proxy
// appends the address it saw to whatever the client sent, so only the last
// entry is the proxy's own observation; the first is attacker-chosen.
func ClientAddress(r *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.Trim(r.RemoteAddr, "[]")
	}
	ip := net.ParseIP(host)
	loopback := ip != nil && ip.IsLoopback()
	if !loopback {
		if host == "" {
			return "", false
		}
		return host, true
	}
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff == "" {
		return "", false
	}
	last := xff
	if i := strings.LastIndex(xff, ","); i >= 0 {
		last = xff[i+1:]
	}
	last = strings.TrimSpace(last)
	if last == "" {
		return "", false
	}
	return last, true
}

// Check implements Backend.
func (l *Limiter) Check(r *http.Request) (string, error) {
	user, presented := attemptedLogin(r)
	var addrKey, userKey string
	if presented {
		if a, ok := ClientAddress(r); ok {
			addrKey = a
		}
		userKey = strings.ToLower(strings.TrimSpace(user))
		wait := l.ByAddress.RetryAfter(addrKey)
		if w := l.ByUser.RetryAfter(userKey); w > wait {
			wait = w
		}
		if wait > 0 {
			return "", &ThrottledError{RetryAfter: wait}
		}
	}
	var errs []error
	for _, b := range l.Backends {
		principal, err := b.Check(r)
		if err == nil {
			return principal, nil
		}
		errs = append(errs, err)
	}
	err := errors.Join(errs...)
	// Only a real bad-credentials rejection counts. A server-side fault
	// (rcptd unreachable, provisioning failed, a bad server token) is not
	// the client's doing and must not lock a legitimate user out while they
	// retry.
	if presented && errors.Is(err, ErrBadCredentials) {
		l.ByAddress.Fail(addrKey)
		l.ByUser.Fail(userKey)
	}
	if len(errs) == 1 {
		return "", errs[0]
	}
	return "", err
}

// Challenge implements Backend.
func (l *Limiter) Challenge(h http.Header, r *http.Request) {
	for _, b := range l.Backends {
		b.Challenge(h, r)
	}
}
