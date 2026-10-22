package auth

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"calserver/internal/metrics"
)

// BasicBackend authenticates HTTP Basic credentials. The validator receives
// the cleartext username/password. Port of sabre's Backend\AbstractBasic.
type BasicBackend struct {
	// Realm is the authentication realm advertised in challenges.
	Realm string
	// PrincipalPrefix is prepended to the username, like sabre.
	PrincipalPrefix string
	// Validate checks a username/password pair.
	Validate func(username, password string) bool
	// Metrics counts attempts and times the check; only requests that
	// actually present Basic credentials are counted, so a Digest login
	// failing this backend in the stacked default setup adds no series.
	Metrics *metrics.Set
}

// principalPath is a backend's principal for username: its PrincipalPrefix,
// by default "principals/", like sabre, plus the username.
func principalPath(prefix, username string) string {
	if prefix == "" {
		prefix = "principals/"
	}
	return prefix + username
}

// basicChallenge adds the Basic challenge for realm, which every Basic
// backend sends.
func basicChallenge(h http.Header, realm string) {
	h.Add("WWW-Authenticate", fmt.Sprintf("Basic realm=%q, charset=\"UTF-8\"", realm))
}

// errNoBasic is a Basic backend's Check failure for a request without Basic
// credentials.
var errNoBasic = errors.New("No 'Authorization: Basic' header found. Either the client didn't send one, or the server is misconfigured")

func (b *BasicBackend) Check(r *http.Request) (string, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return "", errNoBasic
	}
	start := time.Now()
	valid := b.Validate(username, password)
	b.Metrics.ObserveAuthCheck("basic", time.Since(start).Seconds())
	if !valid {
		b.Metrics.AuthAttempt("basic", "bad_credentials")
		return "", ErrBadCredentials
	}
	b.Metrics.AuthAttempt("basic", "ok")
	return principalPath(b.PrincipalPrefix, username), nil
}

func (b *BasicBackend) Challenge(h http.Header, _ *http.Request) { basicChallenge(h, b.Realm) }

// DigestA1 computes md5(username:realm:password) — the digesta1 convention
// shared with sabre's users table (PLAN.md §5). It lets Basic credentials be
// verified against the stored Digest hash.
func DigestA1(username, realm, password string) string {
	sum := md5.Sum([]byte(username + ":" + realm + ":" + password))
	return hex.EncodeToString(sum[:])
}
