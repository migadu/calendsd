package dav

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// listingLogger times a PROPFIND and returns the func that reports it once
// the child count is known. Only the expensive listings are logged: a Depth:1
// on a top-level collection (principals, calendars, addressbooks: one child
// per user, see docs/slow-queries.md) or one that returned more than 1000
// children. The requester and User-Agent say which clients drive them.
func listingLogger(r *http.Request, path string, depth int) func(children int) {
	start := time.Now()
	return func(children int) {
		if depth == 0 {
			return
		}
		root := path != "" && !strings.Contains(path, "/")
		if !root && children <= 1000 {
			return
		}
		slog.InfoContext(r.Context(), "large listing",
			"path", path,
			"principal", Principal(r.Context()),
			"user_agent", r.UserAgent(),
			"children", children,
			"took", time.Since(start))
	}
}
