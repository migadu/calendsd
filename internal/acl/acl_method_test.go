package acl_test

import (
	"net/http/httptest"
	"strings"
	"testing"
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
