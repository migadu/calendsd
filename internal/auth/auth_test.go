package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestBasicBackend(t *testing.T) {
	b := &BasicBackend{
		Realm: "SabreDAV",
		Validate: func(user, pass string) bool {
			return user == "admin" && pass == "admin"
		},
	}

	r := httptest.NewRequest("PROPFIND", "/", nil)
	r.SetBasicAuth("admin", "admin")
	principal, ok, _ := b.Check(r)
	if !ok || principal != "principals/admin" {
		t.Errorf("Check = (%q, %v), want (principals/admin, true)", principal, ok)
	}

	r = httptest.NewRequest("PROPFIND", "/", nil)
	r.SetBasicAuth("admin", "wrong")
	if _, ok, reason := b.Check(r); ok || reason != "Username or password was incorrect" {
		t.Errorf("bad password accepted or wrong reason: %v %q", ok, reason)
	}

	r = httptest.NewRequest("PROPFIND", "/", nil)
	if _, ok, _ := b.Check(r); ok {
		t.Error("missing header accepted")
	}
}

func TestDigestBackend(t *testing.T) {
	const (
		realm    = "SabreDAV"
		username = "admin"
		password = "admin"
		nonce    = "abc123"
		cnonce   = "0a4f113b"
		nc       = "00000001"
		uri      = "/calendars/admin/"
	)
	b := &DigestBackend{
		Realm: realm,
		HashA1: func(r, u string) (string, bool) {
			if u == username {
				return DigestA1(username, r, password), true
			}
			return "", false
		},
	}

	a1 := DigestA1(username, realm, password)
	a2 := md5hex("PROPFIND:" + uri)
	response := md5hex(a1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + a2)

	r := httptest.NewRequest("PROPFIND", uri, nil)
	r.Header.Set("Authorization", fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
		username, realm, nonce, uri, nc, cnonce, response))
	principal, ok, reason := b.Check(r)
	if !ok || principal != "principals/admin" {
		t.Errorf("Check = (%q, %v, %q), want (principals/admin, true)", principal, ok, reason)
	}

	// A tampered response must fail.
	r.Header.Set("Authorization", fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
		username, realm, nonce, uri, nc, cnonce, md5hex("wrong")))
	if _, ok, _ := b.Check(r); ok {
		t.Error("tampered digest response accepted")
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
	if _, ok, reason := b.Check(r); ok || !strings.Contains(reason, "Digest") {
		t.Errorf("missing header: ok=%v reason=%q", ok, reason)
	}

	// A Digest header that carries no username.
	r.Header.Set("Authorization", `Digest realm="SabreDAV", nonce="abc"`)
	if _, ok, _ := b.Check(r); ok {
		t.Error("username-less Digest header accepted")
	}

	// An unknown user gets the same generic message as a wrong password.
	r.Header.Set("Authorization",
		`Digest username="ghost", realm="SabreDAV", nonce="abc", uri="/", qop=auth, nc=00000001, cnonce="x", response="y"`)
	if _, ok, reason := b.Check(r); ok || reason != "Username or password was incorrect" {
		t.Errorf("unknown user: ok=%v reason=%q, want the generic message", ok, reason)
	}

	// The qop-less RFC 2069 form is a downgrade path and must be rejected as
	// a protocol error, not as wrong credentials.
	r.Header.Set("Authorization",
		`Digest username="admin", realm="SabreDAV", nonce="abc", uri="/", response="y"`)
	if _, ok, reason := b.Check(r); ok || reason != "Unsupported digest qop value" {
		t.Errorf("qop-less form: ok=%v reason=%q, want the qop error", ok, reason)
	}
}

// TestDigestChallenge: the 401 challenge advertises Digest with the realm,
// qop=auth, the md5-of-realm opaque, and a fresh random nonce per challenge.
func TestDigestChallenge(t *testing.T) {
	b := &DigestBackend{Realm: "SabreDAV"}
	h := http.Header{}
	b.Challenge(h)
	got := h.Get("WWW-Authenticate")
	if !strings.HasPrefix(got, `Digest realm="SabreDAV",qop="auth",nonce="`) {
		t.Errorf("challenge = %q, want a Digest challenge for realm SabreDAV with qop=auth", got)
	}
	if !strings.Contains(got, `opaque="`+md5hex("SabreDAV")+`"`) {
		t.Errorf("challenge = %q, want opaque=md5(realm)", got)
	}
	h2 := http.Header{}
	b.Challenge(h2)
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
