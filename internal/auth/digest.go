package auth

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
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
	// user, or ok=false if the user is unknown.
	HashA1 func(realm, username string) (hash string, ok bool)
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
	a1, ok := b.HashA1(b.Realm, username)
	if !ok {
		return "", false, "Username or password was incorrect"
	}

	uri := params["uri"]
	a2 := md5hex(r.Method + ":" + uri)
	// Only qop=auth is accepted; the RFC 2069 qop-less form is a
	// downgrade path sabre's Digest also rejects.
	if params["qop"] != "auth" {
		return "", false, "Unsupported digest qop value"
	}
	expected := md5hex(strings.Join([]string{
		a1, params["nonce"], params["nc"], params["cnonce"], params["qop"], a2,
	}, ":"))
	// Constant-time compare so response-hash verification does not leak the
	// expected value through timing. NOTE: this backend is stateless (it does
	// not track or expire the nonce, matching sabre's Digest), so a captured
	// Authorization header can be replayed — deploy behind TLS.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(params["response"])) != 1 {
		return "", false, "Username or password was incorrect"
	}
	return b.prefix() + username, true, ""
}

func (b *DigestBackend) Challenge(h http.Header) {
	nonce := randomHex(12)
	opaque := md5hex(b.Realm)
	h.Add("WWW-Authenticate", fmt.Sprintf(
		`Digest realm="%s",qop="auth",nonce="%s",opaque="%s"`, b.Realm, nonce, opaque))
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
