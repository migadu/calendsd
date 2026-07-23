package auth

import (
	"fmt"
	"net/http/httptest"
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
