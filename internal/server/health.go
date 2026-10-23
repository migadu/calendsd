package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// HealthPath is the unauthenticated liveness/readiness endpoint for load
// balancers and monitoring. Like /.well-known it lives at the root regardless
// of the base URI; the DAV root is read-only, so no resource can shadow it.
const HealthPath = "/health"

// healthTimeout bounds the database ping, so a hung pool answers 503 within
// a typical monitor's own timeout instead of hanging the probe.
const healthTimeout = 2 * time.Second

// WrapHealth answers GET/HEAD on HealthPath with 200 when the server can
// reach its database and 503 when it cannot; every other path goes to h.
// ping nil (no database configured) reports only that the process is up.
// The body never carries the database error: the endpoint is public, so the
// error is logged instead.
func WrapHealth(h http.Handler, ping func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HealthPath {
			h.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		status, body := http.StatusOK, map[string]string{"status": "ok"}
		if ping != nil {
			ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
			defer cancel()
			if err := ping(ctx); err != nil {
				slog.Warn("health check: database unreachable", "err", err)
				status = http.StatusServiceUnavailable
				body = map[string]string{"status": "unavailable", "database": "unreachable"}
			} else {
				body["database"] = "ok"
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(body)
		}
	})
}
