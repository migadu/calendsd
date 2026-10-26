package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// WrapRecover turns a panicking handler into a logged 500. net/http already
// keeps a handler panic from taking the process down, but it answers by
// dropping the connection, which clients report as a network error and retry,
// and it logs through the standard log package outside the structured stream.
// Here the panic and its stack go to slog at Error, and the client gets a 500
// when no response has started yet; once headers are out, the connection is
// aborted as net/http would, since a status can no longer be changed.
//
// Install it innermost, so the access log, request log and metrics wrappers
// see the 500 as an ordinary response rather than a panic.
func WrapRecover(next http.Handler) http.Handler {
	return wrapRecover(next, func(w http.ResponseWriter) {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	})
}

// WrapRecoverJSON is WrapRecover for the contacts API, whose clients parse
// every response as JSON: the 500 carries the same body its own storage
// failures do.
func WrapRecoverJSON(next http.Handler) http.Handler {
	return wrapRecover(next, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"internal error"}`+"\n")
	})
}

func wrapRecover(next http.Handler, respond500 func(http.ResponseWriter)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			// ErrAbortHandler is net/http's sanctioned way to abort a response
			// silently (httputil.ReverseProxy uses it); let it through.
			if err, isErr := p.(error); isErr && errors.Is(err, http.ErrAbortHandler) {
				panic(p)
			}
			slog.ErrorContext(r.Context(), "http handler panic",
				"method", r.Method, "path", r.URL.EscapedPath(),
				"panic", p, "stack", string(debug.Stack()))
			if rec.wrote {
				panic(http.ErrAbortHandler)
			}
			// Drop whatever the handler staged (ETag, Location, ...) so it
			// does not decorate the error.
			clear(rec.Header())
			respond500(rec)
		}()
		next.ServeHTTP(rec, r)
	})
}
