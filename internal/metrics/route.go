package metrics

import "strings"

// knownMethods is the closed DAV verb set used as the method label: the core
// methods plus MKCALENDAR and POST (add-member and the scheduling outbox
// arrive as POST). Anything else — including garbage a scanner sends —
// collapses to "other" so the label can never grow from request data.
var knownMethods = map[string]bool{
	"OPTIONS": true, "GET": true, "HEAD": true, "DELETE": true,
	"PROPFIND": true, "PUT": true, "PROPPATCH": true, "COPY": true,
	"MOVE": true, "REPORT": true, "MKCOL": true, "MKCALENDAR": true,
	"POST": true,
}

// NormalizeMethod maps a request method onto the closed label set.
func NormalizeMethod(m string) string {
	if knownMethods[m] {
		return m
	}
	return "other"
}

// RoutePattern maps a request path onto a fixed pattern set
// (METRICS-PLAN.md §4). path is the escaped URL path as received; baseURI is
// the server's normalized base URI ("/" or "/prefix/"). Classification is by
// path-segment count under the known roots, never by matching user data, so
// the return value is always one of a dozen constants:
//
//	root, well-known, health, principal,
//	calendar-home, calendar, calendar-object,
//	addressbook-home, addressbook, addressbook-object,
//	other
//
// Depths not listed — bare /calendars, proxy principals
// (/principals/{user}/calendar-proxy-read) — classify as "other"
// deliberately; the classifier tests pin those decisions.
func RoutePattern(path, baseURI string) string {
	if path == "" { // e.g. "OPTIONS *"
		return "other"
	}
	// /.well-known lives at the root regardless of the base URI.
	if path == "/.well-known" || strings.HasPrefix(path, "/.well-known/") {
		return "well-known"
	}
	// So does the monitoring endpoint (server.HealthPath).
	if path == "/health" {
		return "health"
	}
	if baseURI == "" {
		baseURI = "/"
	}
	if path == baseURI || path+"/" == baseURI {
		return "root"
	}
	if !strings.HasPrefix(path, baseURI) {
		return "other"
	}
	// Strip at most ONE trailing slash (collections legitimately carry one).
	// A blanket Trim would erase the doubled slashes ("//calendars/hans",
	// "/calendars/hans//") the empty-segment guard below exists to catch.
	rel := strings.TrimSuffix(path[len(baseURI):], "/")
	if rel == "" {
		// Only slashes after the base URI (e.g. "//"): not a real root.
		return "other"
	}
	// RFC 5995 add-member is a POST to {collection};add-member; fold it into
	// its collection's pattern.
	rel = strings.TrimSuffix(rel, ";add-member")

	segs := strings.Split(rel, "/")
	// The middleware runs before any path normalization, so dot segments and
	// doubled slashes ("/calendars/../x") would otherwise count under a real
	// pattern. Traversal-shaped paths are not that route's traffic.
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return "other"
		}
	}
	switch segs[0] {
	case "principals":
		if len(segs) == 2 {
			return "principal"
		}
	case "calendars":
		switch len(segs) {
		case 2:
			return "calendar-home"
		case 3:
			return "calendar"
		case 4:
			return "calendar-object"
		}
	case "addressbooks":
		switch len(segs) {
		case 2:
			return "addressbook-home"
		case 3:
			return "addressbook"
		case 4:
			return "addressbook-object"
		}
	}
	return "other"
}
