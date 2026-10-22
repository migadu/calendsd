package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calserver/internal/dav"
)

// TestDigestA1SeedCompatibility pins the digesta1 convention against the
// value sabre seeds in examples/sql/pgsql.users.sql for admin/admin in realm
// "SabreDAV" (PLAN.md §5: the realm is baked into the hash).
func TestDigestA1SeedCompatibility(t *testing.T) {
	got := DigestA1("admin", "SabreDAV", "admin")
	if want := "87fd274b7b6c01e48d7c2f965da8ddf7"; got != want {
		t.Errorf("DigestA1(admin, SabreDAV, admin) = %s, want %s (sabre seed value)", got, want)
	}
}

// check runs b.Check and reports its outcome as (principal, ok, the message a
// client would see).
func check(b Backend, r *http.Request) (string, bool, string) {
	principal, err := b.Check(r)
	if err != nil {
		return "", false, ClientMessage(err)
	}
	return principal, true, ""
}

func TestBasicBackend(t *testing.T) {
	b := &BasicBackend{
		Realm: "SabreDAV",
		Validate: func(user, pass string) bool {
			return user == "admin" && pass == "admin"
		},
	}

	r := httptest.NewRequest("PROPFIND", "/", nil)
	r.SetBasicAuth("admin", "admin")
	principal, ok, _ := check(b, r)
	if !ok || principal != "principals/admin" {
		t.Errorf("Check = (%q, %v), want (principals/admin, true)", principal, ok)
	}

	r = httptest.NewRequest("PROPFIND", "/", nil)
	r.SetBasicAuth("admin", "wrong")
	if _, ok, reason := check(b, r); ok || reason != "Username or password was incorrect" {
		t.Errorf("bad password accepted or wrong reason: %v %q", ok, reason)
	}

	r = httptest.NewRequest("PROPFIND", "/", nil)
	if _, ok, _ := check(b, r); ok {
		t.Error("missing header accepted")
	}
}

// digestBackend authenticates admin/admin in realm SabreDAV.
func digestBackend() *DigestBackend {
	return &DigestBackend{
		Realm: "SabreDAV",
		HashA1: func(r, u string) (string, bool) {
			if u == "admin" {
				return DigestA1("admin", r, "admin"), true
			}
			return "", false
		},
	}
}

// issueNonce returns the nonce of a fresh challenge from b.
func issueNonce(t *testing.T, b *DigestBackend) string {
	t.Helper()
	h := http.Header{}
	b.Challenge(h, httptest.NewRequest("GET", "/", nil))
	_, rest, _ := strings.Cut(h.Get("WWW-Authenticate"), `nonce="`)
	nonce, _, _ := strings.Cut(rest, `"`)
	return nonce
}

// digestRequest builds a request to method target carrying admin/admin
// credentials for nonce, nc and the given digest uri.
func digestRequest(method, target, uri, nonce, nc, password string) *http.Request {
	const cnonce = "0a4f113b"
	a1 := DigestA1("admin", "SabreDAV", password)
	a2 := md5hex(method + ":" + uri)
	response := md5hex(a1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + a2)
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Authorization", fmt.Sprintf(
		`Digest username="admin", realm="SabreDAV", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
		nonce, uri, nc, cnonce, response))
	return r
}

func TestDigestBackend(t *testing.T) {
	b := digestBackend()
	const uri = "/calendars/admin/"
	nonce := issueNonce(t, b)

	principal, ok, reason := check(b, digestRequest("PROPFIND", uri, uri, nonce, "00000001", "admin"))
	if !ok || principal != "principals/admin" {
		t.Errorf("Check = (%q, %v, %q), want (principals/admin, true)", principal, ok, reason)
	}
	// The same nonce with a higher count is the normal client pattern.
	if _, ok, reason := check(b, digestRequest("PROPFIND", uri, uri, nonce, "00000002", "admin")); !ok {
		t.Errorf("next nonce count rejected: %q", reason)
	}
	// A wrong password must fail.
	if _, ok, _ := check(b, digestRequest("PROPFIND", uri, uri, nonce, "00000003", "wrong")); ok {
		t.Error("wrong password accepted")
	}
}

// TestDigestReplayProtection: a captured request cannot be replayed, pointed
// at another path, or forged with a nonce the server never issued, and an
// expired nonce yields stale=true.
func TestDigestReplayProtection(t *testing.T) {
	b := digestBackend()
	const uri = "/calendars/admin/"
	nonce := issueNonce(t, b)
	captured := digestRequest("PROPFIND", uri, uri, nonce, "00000001", "admin")
	if _, ok, reason := check(b, captured); !ok {
		t.Fatalf("first use rejected: %q", reason)
	}
	if _, ok, _ := check(b, captured); ok {
		t.Error("replayed request accepted")
	}
	if _, ok, _ := check(b, digestRequest("PROPFIND", "/calendars/other/", uri, nonce, "00000005", "admin")); ok {
		t.Error("credentials for one uri accepted on another path")
	}
	if _, ok, _ := check(b, digestRequest("PROPFIND", uri, uri, "abc123", "00000001", "admin")); ok {
		t.Error("unissued nonce accepted")
	}

	later := time.Now().Add(nonceTTL + time.Minute)
	b.now = func() time.Time { return later }
	expired := digestRequest("PROPFIND", uri, uri, nonce, "00000009", "admin")
	if _, ok, _ := check(b, expired); ok {
		t.Error("expired nonce accepted")
	}
	h := http.Header{}
	b.Challenge(h, expired)
	if !strings.HasSuffix(h.Get("WWW-Authenticate"), ",stale=true") {
		t.Errorf("challenge after an expired nonce = %q, want stale=true", h.Get("WWW-Authenticate"))
	}
}

// TestDigestBackendRejections covers the pre-verification failure branches:
// a missing or non-Digest header, a Digest header without a username, an
// unknown user, and the RFC 2069 qop-less downgrade form.
func TestDigestBackendRejections(t *testing.T) {
	b := &DigestBackend{
		Realm: "SabreDAV",
		HashA1: func(r, u string) (string, bool) {
			if u == "admin" {
				return DigestA1("admin", r, "admin"), true
			}
			return "", false
		},
	}

	// No Authorization header at all.
	r := httptest.NewRequest("PROPFIND", "/", nil)
	if _, ok, reason := check(b, r); ok || !strings.Contains(reason, "Digest") {
		t.Errorf("missing header: ok=%v reason=%q", ok, reason)
	}

	// A Digest header that carries no username.
	r.Header.Set("Authorization", `Digest realm="SabreDAV", nonce="abc"`)
	if _, ok, _ := check(b, r); ok {
		t.Error("username-less Digest header accepted")
	}

	// An unknown user gets the same generic message as a wrong password.
	r.Header.Set("Authorization",
		`Digest username="ghost", realm="SabreDAV", nonce="abc", uri="/", qop=auth, nc=00000001, cnonce="x", response="y"`)
	if _, ok, reason := check(b, r); ok || reason != "Username or password was incorrect" {
		t.Errorf("unknown user: ok=%v reason=%q, want the generic message", ok, reason)
	}

	// The qop-less RFC 2069 form is a downgrade path and must be rejected as
	// a protocol error, not as wrong credentials.
	r.Header.Set("Authorization",
		`Digest username="admin", realm="SabreDAV", nonce="abc", uri="/", response="y"`)
	if _, ok, reason := check(b, r); ok || reason != "Unsupported digest qop value" {
		t.Errorf("qop-less form: ok=%v reason=%q, want the qop error", ok, reason)
	}
}

// TestDigestChallenge: the 401 challenge advertises Digest with the realm,
// qop=auth, the md5-of-realm opaque, and a fresh nonce per challenge.
func TestDigestChallenge(t *testing.T) {
	b := &DigestBackend{Realm: "SabreDAV"}
	r := httptest.NewRequest("GET", "/", nil)
	h := http.Header{}
	b.Challenge(h, r)
	got := h.Get("WWW-Authenticate")
	if !strings.HasPrefix(got, `Digest realm="SabreDAV",qop="auth",nonce="`) {
		t.Errorf("challenge = %q, want a Digest challenge for realm SabreDAV with qop=auth", got)
	}
	if !strings.Contains(got, `opaque="`+md5hex("SabreDAV")+`"`) {
		t.Errorf("challenge = %q, want opaque=md5(realm)", got)
	}
	h2 := http.Header{}
	b.Challenge(h2, r)
	if h2.Get("WWW-Authenticate") == got {
		t.Error("two challenges produced the same nonce; want a fresh random one")
	}
}

// TestParseDigestHeaderSkipsBareTokens: a parameter with no "=" is skipped
// instead of aborting the whole parse.
func TestParseDigestHeaderSkipsBareTokens(t *testing.T) {
	params := parseDigestHeader(`Digest username="admin", garbage, realm="r"`)
	if params == nil || params["username"] != "admin" || params["realm"] != "r" {
		t.Errorf("parseDigestHeader = %v, want username and realm parsed around the bare token", params)
	}
}

// TestPrincipalPrefixOverride: every backend substitutes an explicit
// PrincipalPrefix for the "principals/" default.
func TestPrincipalPrefixOverride(t *testing.T) {
	if got := (&BasicBackend{PrincipalPrefix: "users/"}).prefix(); got != "users/" {
		t.Errorf("BasicBackend prefix = %q, want users/", got)
	}
	if got := (&DigestBackend{PrincipalPrefix: "users/"}).prefix(); got != "users/" {
		t.Errorf("DigestBackend prefix = %q, want users/", got)
	}
	if got := (&RcptdBackend{PrincipalPrefix: "users/"}).prefix(); got != "users/" {
		t.Errorf("RcptdBackend prefix = %q, want users/", got)
	}
}

// TestPluginNameInit covers the dav plugin identity surface: the stable name
// and the no-op Init.
func TestPluginNameInit(t *testing.T) {
	p := NewPlugin()
	if got := p.PluginName(); got != "auth" {
		t.Errorf("PluginName = %q, want auth", got)
	}
	p.Init(nil) // no server-side wiring; must not panic
}

// TestFixedPrincipalBypassesBackends: a pre-authenticated plugin (the
// autoLogin test-harness mechanism) lets every request through without
// consulting a backend or emitting a challenge.
func TestFixedPrincipalBypassesBackends(t *testing.T) {
	p := NewLoggedInPlugin("principals/admin")
	w := httptest.NewRecorder()
	proceed, err := p.BeforeMethod(w, httptest.NewRequest("PROPFIND", "/", nil))
	if !proceed || err != nil {
		t.Fatalf("fixed-principal request rejected: proceed=%v err=%v", proceed, err)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("unexpected challenge %q on a pre-authenticated request", got)
	}
}

// TestSubmissionSecretStash: a successful Basic login stashes the plaintext
// password on the request context (so iMIP can submit mail as the user), while
// a successful Digest login — which carries no reusable plaintext — stashes
// nothing.
func TestSubmissionSecretStash(t *testing.T) {
	t.Run("basic stashes the password", func(t *testing.T) {
		p := NewPlugin(&BasicBackend{Realm: "SabreDAV",
			Validate: func(u, pw string) bool { return u == "admin" && pw == "hunter2" }})
		r := httptest.NewRequest("PROPFIND", "/", nil)
		r.SetBasicAuth("admin", "hunter2")
		r = r.WithContext(dav.ContextWithPrincipal(r.Context(), ""))
		if proceed, err := p.BeforeMethod(httptest.NewRecorder(), r); !proceed || err != nil {
			t.Fatalf("Basic login rejected: proceed=%v err=%v", proceed, err)
		}
		if got := dav.SubmissionSecret(r.Context()); got != "hunter2" {
			t.Errorf("stashed secret = %q, want hunter2", got)
		}
	})

	t.Run("digest stashes nothing", func(t *testing.T) {
		b := digestBackend()
		r := digestRequest("PROPFIND", "/", "/", issueNonce(t, b), "00000001", "admin")
		r = r.WithContext(dav.ContextWithPrincipal(r.Context(), ""))
		if proceed, err := NewPlugin(b).BeforeMethod(httptest.NewRecorder(), r); !proceed || err != nil {
			t.Fatalf("Digest login rejected: proceed=%v err=%v", proceed, err)
		}
		if got := dav.SubmissionSecret(r.Context()); got != "" {
			t.Errorf("Digest login stashed secret %q, want empty", got)
		}
	})
}

// TestAutoRequireLoginOff: with AutoRequireLogin disabled an unauthenticated
// request proceeds anonymously instead of drawing a 401 challenge.
func TestAutoRequireLoginOff(t *testing.T) {
	p := NewPlugin(&BasicBackend{Realm: "SabreDAV",
		Validate: func(string, string) bool { return false }})
	p.AutoRequireLogin = false
	w := httptest.NewRecorder()
	proceed, err := p.BeforeMethod(w, httptest.NewRequest("PROPFIND", "/", nil))
	if !proceed || err != nil {
		t.Fatalf("anonymous request rejected with AutoRequireLogin off: proceed=%v err=%v", proceed, err)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("unexpected challenge %q with AutoRequireLogin off", got)
	}
}
