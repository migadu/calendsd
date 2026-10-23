package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"calendsd/internal/metrics"
	"shared/passwd"
)

// RcptdBackend authenticates HTTP Basic credentials against an rcptd daemon's
// /auth endpoint. Unlike an IMAP backend (send password, get yes/no), rcptd is
// a hash *provider*: GET /auth?email=<user> returns the user's Dovecot
// password hashes, and verification happens here, locally, via shared/passwd —
// the same implementation Mizu uses at SMTP AUTH. The plaintext password never
// leaves this process. The passwd source of truth is the same Dovecot file the
// mail stack uses, so DAV logins and mail logins stay in lock-step.
type RcptdBackend struct {
	// Realm is advertised in Basic challenges.
	Realm string
	// PrincipalPrefix is prepended to the (lowercased) login to form the
	// principal path; defaults to "principals/", like BasicBackend.
	PrincipalPrefix string
	// URL is the rcptd base URL, e.g. "http://127.0.0.1:8090". /auth is
	// appended.
	URL string
	// Token is the bearer token rcptd requires (its auth_token). Empty sends
	// no Authorization header.
	Token string
	// HTTP is the client used for /auth lookups; defaults to a 5s-timeout
	// client when nil.
	HTTP *http.Client
	// Provision, when set, is called after a successful verification with the
	// resolved principal path and the login (an email). It must be idempotent
	// — it runs on every cache-missed login. ctx is the request's, so a
	// stalled provisioning write ends with the request.
	Provision func(ctx context.Context, principal, email string) error
	// Logger records unusable stored hashes (a user whose only hash uses an
	// unsupported scheme can never authenticate). Nil disables logging.
	Logger *slog.Logger
	// Metrics counts attempts by typed outcome, times the whole check and the
	// /auth roundtrip separately (disambiguating "rcptd is slow" from "the
	// hash verify is eating the CPU"), and counts credential-cache hits and
	// misses. A nil Set records nothing.
	Metrics *metrics.Set

	// Positive-credential cache: skips the /auth round-trip and the (bcrypt)
	// verification for repeat logins, which chatty CalDAV clients produce in
	// bursts. It stores an HMAC of the credential under a per-process random
	// key, never the password itself. ttl == 0 disables it.
	ttl   time.Duration
	key   []byte
	mu    sync.Mutex
	cache map[string]cacheEntry

	// verifySem bounds concurrent lookup+verify work to the CPU count, so a
	// burst of distinct wrong passwords queues instead of pinning every
	// core with bcrypt and starving the rest of the server (audit 5.8).
	// Acquired under the request context: a caller that gives up waiting
	// is told the service is busy, never that its password was wrong.
	semOnce   sync.Once
	verifySem chan struct{}
	// dummies holds one bcrypt hash per cost seen in real answers, so an
	// unknown user's attempt pays the same verification as a wrong password
	// instead of returning straight from rcptd's 404.
	dummies  map[int]string
	lastCost int
}

// defaultDummyCost is the bcrypt cost used for the unknown-user dummy until
// a real hash has shown the deployment's cost.
const defaultDummyCost = 10

func (b *RcptdBackend) sem() chan struct{} {
	b.semOnce.Do(func() {
		n := runtime.NumCPU()
		if n < 1 {
			n = 1
		}
		b.verifySem = make(chan struct{}, n)
	})
	return b.verifySem
}

// noteCost records the bcrypt cost of a real hash so the dummy matches it.
func (b *RcptdBackend) noteCost(hashes []string) {
	for _, h := range hashes {
		raw := h
		if i := strings.Index(raw, "}"); strings.HasPrefix(raw, "{") && i > 0 {
			raw = raw[i+1:]
		}
		if cost, err := bcrypt.Cost([]byte(raw)); err == nil {
			b.mu.Lock()
			b.lastCost = cost
			b.mu.Unlock()
			return
		}
	}
}

// dummyHash returns a bcrypt hash at the last seen cost, generated once per
// cost.
func (b *RcptdBackend) dummyHash() string {
	b.mu.Lock()
	cost := b.lastCost
	if cost == 0 {
		cost = defaultDummyCost
	}
	h, ok := b.dummies[cost]
	b.mu.Unlock()
	if ok {
		return h
	}
	// Generated outside the lock (shared with the credential cache): at a
	// high cost this takes seconds, and other logins must not wait on it.
	// Two racing first callers both generate; the second write wins, which
	// is harmless. The cost is capped so an absurd stored cost cannot be
	// used to wedge every unknown-user attempt.
	if cost > bcrypt.MaxCost-10 {
		cost = bcrypt.MaxCost - 10
	}
	gen, err := bcrypt.GenerateFromPassword([]byte("calserver-unknown-user-dummy"), cost)
	if err != nil {
		return ""
	}
	b.mu.Lock()
	if b.dummies == nil {
		b.dummies = map[int]string{}
	}
	b.dummies[cost] = string(gen)
	b.mu.Unlock()
	return string(gen)
}

type cacheEntry struct {
	mac    []byte
	expiry time.Time
}

// NewRcptdBackend builds a backend with a positive-auth cache of the given
// TTL (0 disables caching). It panics only if the system CSPRNG is unreadable,
// which is unrecoverable.
func NewRcptdBackend(realm, rcptdURL, token string, ttl time.Duration) *RcptdBackend {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("auth: reading CSPRNG for rcptd cache key: " + err.Error())
	}
	return &RcptdBackend{
		Realm: realm,
		URL:   strings.TrimRight(rcptdURL, "/"),
		Token: token,
		ttl:   ttl,
		key:   key,
		cache: map[string]cacheEntry{},
	}
}

func (b *RcptdBackend) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (b *RcptdBackend) Check(r *http.Request) (string, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return "", errNoBasic
	}
	username = strings.ToLower(username)
	start := time.Now()
	outcome, err := b.verify(r.Context(), username, password)
	b.Metrics.ObserveAuthCheck("rcptd", time.Since(start).Seconds())
	if err != nil {
		b.Metrics.AuthAttempt("rcptd", outcome)
		return "", err
	}
	principal := principalPath(b.PrincipalPrefix, username)
	if b.Provision != nil {
		if err := b.Provision(r.Context(), principal, username); err != nil {
			// Fail closed: an authenticated user with no principal would hit
			// confusing downstream errors, so surface it as an auth failure.
			// Operationally nothing like bad_credentials: a first-login DB
			// write failed, so it gets its own outcome. The database error is
			// logged, never sent to the client.
			b.Metrics.AuthAttempt("rcptd", "provision_error")
			return "", &serverError{msg: "provisioning failed", err: err}
		}
	}
	b.Metrics.AuthAttempt("rcptd", "ok")
	return principal, nil
}

func (b *RcptdBackend) Challenge(h http.Header, _ *http.Request) { basicChallenge(h, b.Realm) }

// verify returns a typed outcome for the attempts metric and, when the
// credentials are not valid, why. "user unknown" and "wrong password"
// deliberately share ErrBadCredentials so the response can't be used to
// enumerate accounts (the metric outcome merges them too — it is aggregate,
// so nothing leaks, but per-user separation stays out on principle).
func (b *RcptdBackend) verify(ctx context.Context, username, password string) (outcome string, err error) {
	if b.cacheHit(username, password) {
		b.Metrics.AuthCacheEvent("hit")
		return "ok", nil
	}
	if b.ttl != 0 {
		// Only a real consultation counts: with the cache disabled (ttl 0) a
		// permanent 0% hit rate would read as a broken cache, not a missing one.
		b.Metrics.AuthCacheEvent("miss")
	}

	hashes, status, err := b.lookup(ctx, username)
	if err != nil {
		return "unavailable", &serverError{msg: "authentication service is unreachable",
			err: errors.Join(ErrUnavailable, err)}
	}
	// The semaphore bounds only the CPU work (bcrypt), never the rcptd
	// round trip above: holding it across a slow rcptd would turn a slow
	// lookup service into an auth outage for everyone.
	select {
	case b.sem() <- struct{}{}:
		defer func() { <-b.verifySem }()
	case <-ctx.Done():
		return "unavailable", ErrUnavailable
	}
	switch status {
	case http.StatusOK:
		b.noteCost(hashes)
		// fall through to verification
	case http.StatusNotFound:
		// Pay the same bcrypt work as a wrong password, so the time to a
		// rejection does not say whether the user exists.
		if dummy := b.dummyHash(); dummy != "" {
			_ = bcrypt.CompareHashAndPassword([]byte(dummy), []byte(password))
		}
		return "bad_credentials", ErrBadCredentials
	case http.StatusServiceUnavailable:
		// Transient CDB fault on rcptd's side; retryable, not a definitive no.
		return "unavailable", ErrUnavailable
	case http.StatusUnauthorized:
		// Our own bearer token is wrong — an operator misconfiguration.
		return "error", errors.New("authentication service rejected this server's credentials")
	default:
		return "error", fmt.Errorf("authentication service returned status %d", status)
	}

	res := passwd.VerifyAny(hashes, password)
	if b.Logger != nil && (res.Unsupported > 0 || len(res.Malformed) > 0) {
		b.Logger.Warn("rcptd auth: unusable stored hash(es)",
			"user", username, "unsupported", res.Unsupported, "malformed", res.Malformed)
	}
	if !res.Matched {
		return "bad_credentials", ErrBadCredentials
	}
	b.cacheStore(username, password)
	return "ok", nil
}

// authResponse is the subset of rcptd's /auth JSON we consume. allowed_from
// (send-as authorization) is meaningless for CalDAV and intentionally ignored.
type authResponse struct {
	PasswordHashes []string `json:"password_hashes"`
}

func (b *RcptdBackend) lookup(ctx context.Context, email string) (hashes []string, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.URL+"/auth?email="+url.QueryEscape(email), nil)
	if err != nil {
		return nil, 0, err
	}
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	start := time.Now()
	resp, err := b.client().Do(req)
	if err != nil {
		b.Metrics.RcptdRequest("error", time.Since(start).Seconds())
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a bounded amount so the connection can be reused.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		b.Metrics.RcptdRequest(rcptdCodeLabel(resp.StatusCode), time.Since(start).Seconds())
		return nil, resp.StatusCode, nil
	}
	var ar authResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ar); err != nil {
		// A 200 whose body doesn't decode (a proxy error page, a broken
		// rcptd) fails every login; counting it as code="200" would keep the
		// rcptd health alert green through the outage.
		b.Metrics.RcptdRequest("error", time.Since(start).Seconds())
		return nil, 0, fmt.Errorf("decoding /auth response: %w", err)
	}
	b.Metrics.RcptdRequest("200", time.Since(start).Seconds())
	return ar.PasswordHashes, http.StatusOK, nil
}

// rcptdCodeLabel keeps the code label a closed set: the statuses verify()
// distinguishes stay literal, anything else — the remote server chooses the
// status, so it must not mint series — collapses to "other".
func rcptdCodeLabel(status int) string {
	switch status {
	case http.StatusOK, http.StatusNotFound, http.StatusUnauthorized, http.StatusServiceUnavailable:
		return strconv.Itoa(status)
	}
	return "other"
}

// mac binds an email+password pair to the per-process key. The NUL separator
// keeps ("ab","c") distinct from ("a","bc").
func (b *RcptdBackend) mac(email, password string) []byte {
	h := hmac.New(sha256.New, b.key)
	h.Write([]byte(email))
	h.Write([]byte{0})
	h.Write([]byte(password))
	return h.Sum(nil)
}

func (b *RcptdBackend) cacheHit(email, password string) bool {
	if b.ttl == 0 {
		return false
	}
	want := b.mac(email, password)
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.cache[email]
	if !ok || time.Now().After(e.expiry) {
		return false
	}
	return subtle.ConstantTimeCompare(e.mac, want) == 1
}

func (b *RcptdBackend) cacheStore(email, password string) {
	if b.ttl == 0 {
		return
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	// Opportunistically drop expired entries so a churn of distinct logins
	// can't grow the map without bound.
	for k, e := range b.cache {
		if now.After(e.expiry) {
			delete(b.cache, k)
		}
	}
	b.cache[email] = cacheEntry{mac: b.mac(email, password), expiry: now.Add(b.ttl)}
}
