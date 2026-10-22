package auth

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"calserver/internal/metrics"
)

// DigestBackend authenticates HTTP Digest credentials (RFC 2617, MD5 +
// qop=auth) against stored A1 hashes — the digesta1 column convention shared
// with sabre. Port of sabre's Backend\AbstractDigest + sabre/http's Digest.
type DigestBackend struct {
	// Realm must match the realm baked into the stored A1 hashes.
	Realm string
	// PrincipalPrefix is prepended to the username, like sabre.
	PrincipalPrefix string
	// HashA1 returns the stored md5(user:realm:pass) hex digest for a
	// user, "" if the user is unknown, or an error if the store failed.
	HashA1 func(realm, username string) (hash string, err error)
	// Metrics counts attempts and times the check; only requests that
	// actually present Digest credentials are counted.
	Metrics *metrics.Set

	keyOnce sync.Once
	key     []byte // signs nonces; see newNonce
	mu      sync.Mutex
	counts  map[string]nonceCount // highest nc seen per live nonce
	now     func() time.Time      // test clock; nil means time.Now
}

func (b *DigestBackend) prefix() string {
	if b.PrincipalPrefix != "" {
		return b.PrincipalPrefix
	}
	return "principals/"
}

func (b *DigestBackend) Check(r *http.Request) (string, bool, string) {
	params := parseDigestHeader(r.Header.Get("Authorization"))
	if params == nil {
		return "", false, "No 'Authorization: Digest' header found. Either the client didn't send one, or the server is misconfigured"
	}
	username := params["username"]
	if username == "" {
		return "", false, "No 'Authorization: Digest' header found. Either the client didn't send one, or the server is misconfigured"
	}
	start := time.Now()
	defer func() { b.Metrics.ObserveAuthCheck("digest", time.Since(start).Seconds()) }()
	a1, err := b.HashA1(b.Realm, username)
	if err != nil {
		// An outage, not wrong credentials: "error", so it doesn't read as a
		// brute force.
		b.Metrics.AuthAttempt("digest", "error")
		return "", false, "Authentication backend unavailable"
	}
	if a1 == "" {
		b.Metrics.AuthAttempt("digest", "bad_credentials")
		return "", false, "Username or password was incorrect"
	}

	// Only qop=auth is accepted; the RFC 2069 qop-less form is a
	// downgrade path sabre's Digest also rejects.
	if params["qop"] != "auth" {
		// A protocol problem, not wrong credentials: "error", so a
		// misbehaving client doesn't read as a brute force.
		b.Metrics.AuthAttempt("digest", "error")
		return "", false, "Unsupported digest qop value"
	}
	issued, valid := b.verifyNonce(params["nonce"])
	if !valid {
		b.Metrics.AuthAttempt("digest", "error")
		return "", false, "Invalid digest nonce"
	}
	// The digest covers the uri parameter, not the request line, so without
	// this a captured header would authorize any other path.
	uri := params["uri"]
	if uri != r.RequestURI && uri != r.URL.RequestURI() {
		b.Metrics.AuthAttempt("digest", "error")
		return "", false, "Digest uri does not match the request"
	}
	a2 := md5hex(r.Method + ":" + uri)
	expected := md5hex(strings.Join([]string{
		a1, params["nonce"], params["nc"], params["cnonce"], params["qop"], a2,
	}, ":"))
	// Constant-time compare so response-hash verification does not leak the
	// expected value through timing.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(params["response"])) != 1 {
		b.Metrics.AuthAttempt("digest", "bad_credentials")
		return "", false, "Username or password was incorrect"
	}
	// Right password, old nonce: Challenge answers with stale=true, so the
	// client retries with a fresh nonce without prompting the user.
	if b.clock().Sub(issued) > nonceTTL {
		b.Metrics.AuthAttempt("digest", "error")
		return "", false, "Digest nonce expired"
	}
	if !b.advanceNonceCount(params["nonce"], params["nc"], issued) {
		b.Metrics.AuthAttempt("digest", "error")
		return "", false, "Digest nonce count reused"
	}
	b.Metrics.AuthAttempt("digest", "ok")
	return b.prefix() + username, true, ""
}

// nonceTTL is how long an issued nonce is accepted. Clients reuse a nonce
// across requests, so an expiry only costs them one stale=true round trip.
const nonceTTL = 5 * time.Minute

// newNonce issues "<unix-hex>.<random-hex>.<mac>", signed with a key that
// lives as long as the process, so the server can tell its own unexpired
// nonces apart without storing them.
func (b *DigestBackend) newNonce() string {
	body := strconv.FormatInt(b.clock().Unix(), 16) + "." + randomHex(8)
	return body + "." + b.nonceMAC(body)
}

// verifyNonce reports whether nonce was issued by this process and when.
func (b *DigestBackend) verifyNonce(nonce string) (time.Time, bool) {
	i := strings.LastIndexByte(nonce, '.')
	if i < 0 {
		return time.Time{}, false
	}
	body, mac := nonce[:i], nonce[i+1:]
	if subtle.ConstantTimeCompare([]byte(mac), []byte(b.nonceMAC(body))) != 1 {
		return time.Time{}, false
	}
	ts, _, _ := strings.Cut(body, ".")
	unix, err := strconv.ParseInt(ts, 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(unix, 0), true
}

func (b *DigestBackend) nonceMAC(body string) string {
	b.keyOnce.Do(func() { b.key = []byte(randomHex(32)) })
	mac := hmac.New(sha256.New, b.key)
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// advanceNonceCount accepts nc only if it is higher than any count already
// used with this nonce, so a captured request cannot be replayed while its
// nonce is still valid. Counts for expired nonces are dropped as it goes.
func (b *DigestBackend) advanceNonceCount(nonce, ncHex string, issued time.Time) bool {
	nc, err := strconv.ParseUint(ncHex, 16, 64)
	if err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.counts == nil {
		b.counts = map[string]nonceCount{}
	}
	if len(b.counts) >= 1024 {
		now := b.clock()
		for n, c := range b.counts {
			if now.Sub(c.issued) > nonceTTL {
				delete(b.counts, n)
			}
		}
	}
	if last, seen := b.counts[nonce]; seen && nc <= last.nc {
		return false
	}
	b.counts[nonce] = nonceCount{nc: nc, issued: issued}
	return true
}

type nonceCount struct {
	nc     uint64
	issued time.Time
}

func (b *DigestBackend) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *DigestBackend) Challenge(h http.Header, r *http.Request) {
	stale := ""
	if params := parseDigestHeader(r.Header.Get("Authorization")); params != nil {
		if issued, ok := b.verifyNonce(params["nonce"]); ok && b.clock().Sub(issued) > nonceTTL {
			stale = ",stale=true"
		}
	}
	opaque := md5hex(b.Realm)
	h.Add("WWW-Authenticate", fmt.Sprintf(
		`Digest realm="%s",qop="auth",nonce="%s",opaque="%s"%s`, b.Realm, b.newNonce(), opaque, stale))
}

// parseDigestHeader parses `Digest key="value", key=value, ...` into a map,
// or nil if the header is not a Digest header.
func parseDigestHeader(header string) map[string]string {
	const prefix = "digest "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return nil
	}
	out := map[string]string{}
	for _, part := range splitDigestParams(header[len(prefix):]) {
		k, v, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

// splitDigestParams splits on commas outside quoted strings.
func splitDigestParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for _, c := range s {
		switch {
		case c == '"':
			inQuotes = !inQuotes
			cur.WriteRune(c)
		case c == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(buf)
}
