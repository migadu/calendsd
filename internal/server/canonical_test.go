package server

import (
	"strings"
	"testing"
)

// Every stored digesta1 bakes in the realm, so changing the default would
// lock every user out.
func TestDefaultRealmIsPinned(t *testing.T) {
	if DefaultRealm != "SabreDAV" {
		t.Fatalf("DefaultRealm = %q; stored hashes were minted under SabreDAV", DefaultRealm)
	}
}

// Canonical is compared as a string across two processes (ADMIN-PLAN.md
// §2); equivalent configurations must render identical lines.
func TestCanonicalNormalizes(t *testing.T) {
	variants := []Config{
		{BaseURI: "/dav", Realm: "SabreDAV"},
		{BaseURI: "/dav/", Realm: "SabreDAV"},
		{BaseURI: "dav", Realm: "SabreDAV"},
		{BaseURI: "/dav/"}, // realm defaulted by compose
	}
	want := variants[0].Canonical()
	for _, v := range variants[1:] {
		if got := v.Canonical(); got != want {
			t.Errorf("Canonical(%+v) = %q, want %q", v, got, want)
		}
	}
	if empty := (Config{}).Canonical(); empty == want {
		t.Error("root and /dav configs must differ")
	}
}

// TestCanonicalIMipDurable: the echo reflects whether a credential key is
// configured (so ops can confirm the durable per-user path is on) but never
// leaks the key bytes themselves.
func TestCanonicalIMipDurable(t *testing.T) {
	secret := []byte("this-is-a-32-byte-secret-key!!!!")
	on := Config{IMipQueue: IMipQueueConfig{CredentialKey: secret}}.Canonical()
	off := Config{}.Canonical()
	if !strings.Contains(on, "imip-durable=true") {
		t.Errorf("Canonical with key = %q, want imip-durable=true", on)
	}
	if !strings.Contains(off, "imip-durable=false") {
		t.Errorf("Canonical without key = %q, want imip-durable=false", off)
	}
	if strings.Contains(on, string(secret)) {
		t.Error("Canonical leaked the credential key bytes")
	}
}
