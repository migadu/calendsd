package server

import "testing"

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
