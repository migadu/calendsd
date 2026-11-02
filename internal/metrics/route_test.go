package metrics

import "testing"

func TestRoutePattern(t *testing.T) {
	tests := []struct {
		path, base, want string
	}{
		{"/", "/", "root"},
		{"/.well-known/caldav", "/", "well-known"},
		{"/.well-known/carddav", "/", "well-known"},
		{"/.well-known", "/", "well-known"},
		{"/health", "/", "health"},
		{"/health", "/dav/", "health"},
		{"/health/x", "/", "other"},
		{"/principals/hans", "/", "principal"},
		{"/principals/hans/", "/", "principal"},
		{"/calendars/hans", "/", "calendar-home"},
		{"/calendars/hans/", "/", "calendar-home"},
		{"/calendars/hans/default", "/", "calendar"},
		{"/calendars/hans/default/abc.ics", "/", "calendar-object"},
		{"/calendars/hans/default%20cal/a%40b.ics", "/", "calendar-object"},
		{"/addressbooks/hans", "/", "addressbook-home"},
		{"/addressbooks/hans/contacts", "/", "addressbook"},
		{"/addressbooks/hans/contacts/x.vcf", "/", "addressbook-object"},

		// RFC 5995 add-member folds into its collection's pattern.
		{"/calendars/hans/default;add-member", "/", "calendar"},
		{"/addressbooks/hans/contacts;add-member", "/", "addressbook"},

		// Pinned decisions: bare roots and proxy principals are "other",
		// not accidents (docs/design/METRICS-PLAN.md §4).
		{"/calendars", "/", "other"},
		{"/addressbooks", "/", "other"},
		{"/principals", "/", "other"},
		{"/principals/hans/calendar-proxy-read", "/", "other"},
		{"/principals/hans/calendar-proxy-write", "/", "other"},

		// Hostile / weird paths never invent a new label.
		{"/calendars/hans/default/a/b/c", "/", "other"},
		{"/calendars/../etc/passwd", "/", "other"},
		{"/%2e%2e/calendars/hans", "/", "other"},
		{"", "/", "other"},
		{"/random", "/", "other"},
		{"//calendars/hans", "/", "other"},
		{"/calendars/hans//", "/", "other"},
		{"/calendars//hans", "/", "other"},
		{"//", "/", "other"},

		// Non-root base URI.
		{"/dav/", "/dav/", "root"},
		{"/dav", "/dav/", "root"},
		{"/dav/calendars/hans/default", "/dav/", "calendar"},
		{"/calendars/hans", "/dav/", "other"},
		{"/.well-known/caldav", "/dav/", "well-known"},
	}
	for _, tt := range tests {
		if got := RoutePattern(tt.path, tt.base); got != tt.want {
			t.Errorf("RoutePattern(%q, %q) = %q, want %q", tt.path, tt.base, got, tt.want)
		}
	}
}

func TestNormalizeMethod(t *testing.T) {
	for _, m := range []string{"PROPFIND", "REPORT", "PUT", "POST", "MKCALENDAR"} {
		if got := NormalizeMethod(m); got != m {
			t.Errorf("NormalizeMethod(%q) = %q", m, got)
		}
	}
	for _, m := range []string{"propfind", "TRACE", "PATCH", "EVIL\nMETHOD", ""} {
		if got := NormalizeMethod(m); got != "other" {
			t.Errorf("NormalizeMethod(%q) = %q, want other", m, got)
		}
	}
}
