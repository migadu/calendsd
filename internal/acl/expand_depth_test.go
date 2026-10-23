package acl_test

// Audit 5.1: expand-property recursed once per nested <property> with no
// depth limit; the nesting is request-driven and each level fans out over
// every href the level above returned.

import (
	"strings"
	"testing"
)

// nestedExpand builds an expand-property body nesting principal-URL
// `levels` deep: a principal's principal-URL is itself, so every level
// re-expands the same node — the cycle shape the limit exists for.
func nestedExpand(levels int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><d:expand-property xmlns:d="DAV:">`)
	for i := 0; i < levels; i++ {
		b.WriteString(`<d:property name="principal-URL">`)
	}
	b.WriteString(`<d:property name="displayname"/>`)
	for i := 0; i < levels; i++ {
		b.WriteString(`</d:property>`)
	}
	b.WriteString(`</d:expand-property>`)
	return b.String()
}

func TestExpandPropertyDepthLimit(t *testing.T) {
	srv := searchServer()
	// principal-URL of a principal is itself: a self-cycle that terminates
	// only because the nesting does. Eight levels completes...
	w := report(t, srv, "/principals/admin/", nestedExpand(8))
	if w.Code != 207 || !strings.Contains(w.Body.String(), "Administrator") {
		t.Fatalf("8 levels: status = %d:\n%s", w.Code, w.Body.String())
	}
	// ...nine is refused outright.
	w = report(t, srv, "/principals/admin/", nestedExpand(9))
	if w.Code != 400 {
		t.Fatalf("9 levels: status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}
