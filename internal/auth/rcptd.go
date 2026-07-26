package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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
	// — it runs on every cache-missed login.
	Provision func(principal, email string) error
	// Logger records unusable stored hashes (a user whose only hash uses an
	// unsupported scheme can never authenticate). Nil disables logging.
	Logger *slog.Logger

	// Positive-credential cache: skips the /auth round-trip and the (bcrypt)
	// verification for repeat logins, which chatty CalDAV clients produce in
	// bursts. It stores an HMAC of the credential under a per-process random
	// key, never the password itself. ttl == 0 disables it.
	ttl   time.Duration
	key   []byte
	mu    sync.Mutex
	cache map[string]cacheEntry
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

func (b *RcptdBackend) prefix() string {
	if b.PrincipalPrefix != "" {
		return b.PrincipalPrefix
	}
	return "principals/"
}

func (b *RcptdBackend) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (b *RcptdBackend) Check(r *http.Request) (string, bool, string) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return "", false, "No 'Authorization: Basic' header found. Either the client didn't send one, or the server is misconfigured"
	}
	username = strings.ToLower(username)
	if ok, reason := b.verify(r.Context(), username, password); !ok {
		return "", false, reason
	}
	principal := b.prefix() + username
	if b.Provision != nil {
		if err := b.Provision(principal, username); err != nil {
			// Fail closed: an authenticated user with no principal would hit
			// confusing downstream errors, so surface it as an auth failure.
			return "", false, "provisioning failed: " + err.Error()
		}
	}
	return principal, true, ""
}

func (b *RcptdBackend) Challenge(h http.Header) {
	h.Add("WWW-Authenticate", fmt.Sprintf("Basic realm=%q, charset=\"UTF-8\"", b.Realm))
}

// verify returns whether the credentials are valid, plus a client-facing
// reason on failure. "user unknown" and "wrong password" deliberately share
// one message so the response can't be used to enumerate accounts.
func (b *RcptdBackend) verify(ctx context.Context, username, password string) (bool, string) {
	const bad = "Username or password was incorrect"

	if b.cacheHit(username, password) {
		return true, ""
	}

	hashes, status, err := b.lookup(ctx, username)
	if err != nil {
		return false, "authentication service is unreachable"
	}
	switch status {
	case http.StatusOK:
		// fall through to verification
	case http.StatusNotFound:
		return false, bad
	case http.StatusServiceUnavailable:
		// Transient CDB fault on rcptd's side; retryable, not a definitive no.
		return false, "authentication service is temporarily unavailable"
	case http.StatusUnauthorized:
		// Our own bearer token is wrong — an operator misconfiguration.
		return false, "authentication service rejected this server's credentials"
	default:
		return false, fmt.Sprintf("authentication service returned status %d", status)
	}

	res := passwd.VerifyAny(hashes, password)
	if b.Logger != nil && (res.Unsupported > 0 || len(res.Malformed) > 0) {
		b.Logger.Warn("rcptd auth: unusable stored hash(es)",
			"user", username, "unsupported", res.Unsupported, "malformed", res.Malformed)
	}
	if !res.Matched {
		return false, bad
	}
	b.cacheStore(username, password)
	return true, ""
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
	resp, err := b.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a bounded amount so the connection can be reused.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, resp.StatusCode, nil
	}
	var ar authResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ar); err != nil {
		return nil, 0, fmt.Errorf("decoding /auth response: %w", err)
	}
	return ar.PasswordHashes, http.StatusOK, nil
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
