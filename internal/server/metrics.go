package server

import (
	"bytes"
	"net/http"
	"time"

	"calserver/internal/metrics"
)

// WrapMetrics records request count, duration, and in-flight gauge for every
// request. It wraps outside WrapWellKnown so discovery redirects are counted
// too (METRICS-PLAN.md §3). A nil set returns next unchanged.
func WrapMetrics(next http.Handler, set *metrics.Set, baseURI string) http.Handler {
	if set == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		set.HTTPInFlightInc()
		defer set.HTTPInFlightDec()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		// Deferred so a panicking handler is still counted (as a 500) — a
		// crashing request class must not read as a healthy dashboard. The
		// panic is rethrown, preserving net/http's own recovery semantics.
		defer func() {
			code := rec.status
			p := recover()
			if p != nil {
				code = http.StatusInternalServerError
			}
			set.ObserveHTTPRequest(
				metrics.NormalizeMethod(r.Method),
				metrics.RoutePattern(r.URL.EscapedPath(), baseURI),
				code, time.Since(start).Seconds())
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// statusRecorder captures the response status and byte count for the
// metrics, access-log and request-log wrappers. When body is non-nil (the
// request log only, a debug aid) it also keeps the first max bytes of the
// response body.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	wrote   bool
	written int // total response bytes, including any past the body cap
	body    *bytes.Buffer
	max     int
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.wrote = true // an implicit 200 if WriteHeader was never called
	if r.body != nil {
		if room := r.max - r.body.Len(); room > 0 {
			r.body.Write(p[:min(len(p), room)])
		}
	}
	n, err := r.ResponseWriter.Write(p)
	r.written += n
	return n, err
}

// Flush preserves streaming for clients that rely on it (the real writer is
// a Flusher); a no-op otherwise.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer through the
// recorder (deadlines, hijacking, full-duplex).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
