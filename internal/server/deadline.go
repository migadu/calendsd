package server

import (
	"context"
	"net/http"
	"time"
)

// RequestDeadline bounds one request's work. The listener's read and write
// timeouts (cmd/calserver) bound the bytes on the wire; this bounds what the
// handler does with them, through the request context: rcptd lookups, first-
// login provisioning, the job enqueue and every storage call that takes the
// context. It is what lets the wire timeouts be raised for large exports
// without a slow client or a slow query holding a handler indefinitely
// (audit 6.9).
const RequestDeadline = 300 * time.Second

// WrapRequestDeadline installs RequestDeadline on each request's context.
func WrapRequestDeadline(next http.Handler, d time.Duration) http.Handler {
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
