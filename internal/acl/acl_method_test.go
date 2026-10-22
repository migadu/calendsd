package acl_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"calendsd/internal/acl"
)

// TestACLMethodEmptyBody: an ACL request with no body is a 400, matching
// sabre's httpAcl "XML body expected in ACL request".
func TestACLMethodEmptyBody(t *testing.T) {
	w := request(t, buildServer("principals/admin"), "ACL", "/calendars/user1/cal/", "")
	if w.Code != 400 {
		t.Errorf("ACL empty body = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

// TestACLMethodForbiddenOnFixedACL: a well-formed ACL request against a node
// with a fixed, owner-derived ACL returns 403 (setACL unsupported) — the same
// response stock sabre's ACLTrait produces — rather than the old 501.
func TestACLMethodForbiddenOnFixedACL(t *testing.T) {
	body := `<?xml version="1.0"?><d:acl xmlns:d="DAV:"><d:ace>` +
		`<d:principal><d:href>/principals/user2/</d:href></d:principal>` +
		`<d:grant><d:privilege><d:read/></d:privilege></d:grant>` +
		`</d:ace></d:acl>`
	w := request(t, buildServer("principals/admin"), "ACL", "/calendars/user1/cal/", body)
	if w.Code != 403 {
		t.Errorf("ACL on fixed-ACL node = %d, want 403 (setACL unsupported):\n%s", w.Code, w.Body.String())
	}
}

// TestACLForNodeIsEffectiveACL: ACLForNode reports the ACL privilege checks
// evaluate — the default ACL for a node without its own, and the
// authenticated-read grant on principals when that option is on — not only
// the node's own ACEs.
func TestACLForNodeIsEffectiveACL(t *testing.T) {
	srv := buildServer("principals/admin")
	plugin := srv.Plugin("acl").(*acl.Plugin)
	plugin.PrincipalsReadableByAuthenticated = true

	hasAuthRead := func(aces []acl.ACE) bool {
		for _, ace := range aces {
			if ace.Privilege == "{DAV:}read" && ace.Principal == "{DAV:}authenticated" {
				return true
			}
		}
		return false
	}
	for _, path := range []string{"", "principals/user1"} {
		node, err := srv.Tree.NodeForPath(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		if got := plugin.ACLForNode(node); !hasAuthRead(got) {
			t.Errorf("ACLForNode(%q) = %+v, want {DAV:}read for {DAV:}authenticated", path, got)
		}
	}
}

// TestACLMethodNotFound: ACL on a missing node is a 404.
func TestACLMethodNotFound(t *testing.T) {
	body := `<?xml version="1.0"?><d:acl xmlns:d="DAV:"/>`
	w := request(t, buildServer("principals/admin"), "ACL", "/calendars/user1/nope/", body)
	if w.Code != 404 {
		t.Errorf("ACL on missing node = %d, want 404:\n%s", w.Code, w.Body.String())
	}
}

// TestSupportedPrivilegeSet: the {DAV:}supported-privilege-set property is now
// emitted (was a 404 divergence), carrying the node's privilege tree including
// the calendar-specific read-free-busy.
func TestSupportedPrivilegeSet(t *testing.T) {
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:supported-privilege-set/></d:prop></d:propfind>`
	req := httptest.NewRequest("PROPFIND", "/calendars/user1/cal/", strings.NewReader(body))
	req.Header.Set("Depth", "0")
	w := httptest.NewRecorder()
	buildServer("principals/admin").ServeHTTP(w, req)

	if w.Code != 207 {
		t.Fatalf("PROPFIND status = %d:\n%s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	for _, want := range []string{"supported-privilege-set", "supported-privilege", "read-free-busy", "abstract"} {
		if !strings.Contains(out, want) {
			t.Errorf("supported-privilege-set output missing %q:\n%s", want, out)
		}
	}
	// The property must not report 404 for this node.
	if strings.Contains(out, "supported-privilege-set/></d:prop>\n   <d:status>HTTP/1.1 404") {
		t.Errorf("supported-privilege-set still 404:\n%s", out)
	}
}
