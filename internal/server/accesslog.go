package server

import (
	"log/slog"
	"net/http"
	"time"
)

// WrapAccessLog logs one structured slog line per request — method, path,
// status, response bytes, and duration — through the process-wide logger. That
// puts the access log in the same stream as the rest of the server's output,
// so whatever handles the main log (rotation, journald, a log shipper) handles
// it too. Contrast WrapRequestLog, which dumps full headers and bodies to one
// file per request under its own directory: a heavyweight debug aid with no
// rotation, unsafe to leave on. This summary is light enough to run
// continuously. Disabled by default (enabled is false); turn it on with
// [logging].access_log or -access-log.
//
// The line's slog level tracks the status class so failures surface even when
// the level is raised past Info to mute the success lines: 5xx logs at Error,
// other 4xx at Warn, and a bare 401 stays at Info because it is the routine
// first half of the Basic/Digest challenge, not a failure.
func WrapAccessLog(next http.Handler, enabled bool) http.Handler {
	return wrapAccessLog(next, enabled, func(r *http.Request) string { return r.URL.RequestURI() })
}

// WrapAccessLogPathOnly is WrapAccessLog without the query string, for the
// contacts API: its queries are what the user typed (search terms, an address
// per suggestion keystroke), which an always-on access log must not collect.
func WrapAccessLogPathOnly(next http.Handler, enabled bool) http.Handler {
	return wrapAccessLog(next, enabled, func(r *http.Request) string { return r.URL.EscapedPath() })
}

func wrapAccessLog(next http.Handler, enabled bool, path func(*http.Request) string) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		// Deferred so a panicking handler is still logged (as a 500) before the
		// panic unwinds; it is rethrown to preserve net/http's recovery.
		defer func() {
			status := rec.status
			if p := recover(); p != nil {
				logAccess(r, path(r), http.StatusInternalServerError, rec.written, time.Since(start))
				panic(p)
			}
			logAccess(r, path(r), status, rec.written, time.Since(start))
		}()
		next.ServeHTTP(rec, r)
	})
}

func logAccess(r *http.Request, path string, status, bytes int, dur time.Duration) {
	level := slog.LevelInfo
	switch {
	case status >= 500:
		level = slog.LevelError
	case status == http.StatusUnauthorized:
		// routine auth challenge, not a failure — keep it out of the warn stream
	case status >= 400:
		level = slog.LevelWarn
	}
	slog.LogAttrs(r.Context(), level, "http request",
		slog.String("method", r.Method),
		slog.String("path", path),
		slog.Int("status", status),
		slog.Int("bytes", bytes),
		slog.Duration("dur", dur),
		slog.String("remote", r.RemoteAddr),
	)
}
