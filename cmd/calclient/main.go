// calclient serves the demo web client and reverse-proxies DAV requests to a
// calserver, so the browser talks to one origin and CORS never comes into
// play (CalDAV needs PROPFIND/REPORT/MKCALENDAR/PROPPATCH/MOVE, which a
// cross-origin page cannot send without preflight support the server does
// not offer).
package main

import (
	"embed"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"calserver/internal/logging"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8091", "listen address for the client UI and proxy")
	target := flag.String("target", "http://127.0.0.1:8080", "base URL of the calserver to proxy DAV requests to")
	devAssets := flag.String("dev-assets", "", "serve client assets from this directory instead of the embedded copy (for iterating without rebuilds)")
	logLevel := flag.String("log-level", "", "log verbosity: debug, info, warn, or error (default info). When empty, $CALSERVER_LOG_LEVEL applies")
	flag.Parse()

	if err := logging.Setup(*logLevel); err != nil {
		slog.Error("configuring logging", "err", err)
		os.Exit(1)
	}

	targetURL, err := url.Parse(*target)
	if err != nil || targetURL.Scheme == "" || targetURL.Host == "" {
		slog.Error("invalid -target", "target", *target, "err", err)
		os.Exit(1)
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Strip the auth challenge on proxied 401s. The JS login form owns
		// authentication, and a WWW-Authenticate: Basic header makes the
		// browser try to satisfy the Basic challenge itself: on a fetch()
		// with rejected credentials Chrome then hangs the request forever
		// instead of resolving the promise, so a wrong password or a stale
		// saved session freezes the app. Removing the header lets fetch see
		// the 401 immediately.
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Header.Del("WWW-Authenticate")
		}
		return nil
	}

	var assets http.FileSystem
	if *devAssets != "" {
		assets = http.Dir(*devAssets)
	} else {
		sub, err := fs.Sub(webFS, "web")
		if err != nil {
			slog.Error("embedded assets", "err", err)
			os.Exit(1)
		}
		assets = http.FS(sub)
	}

	mux := http.NewServeMux()
	// "GET /{$}" matches only the exact root: a plain "GET /" pattern is a
	// subtree wildcard that would swallow every GET (event .ics, ?export,
	// /.well-known/*) meant for the proxy. The static URL paths mirror the
	// web/ directory layout, so one FileServer covers all three mounts:
	// /cal/ (end-user app), /app/ (protocol explorer), /shared/ (modules).
	static := http.FileServer(assets)
	mux.Handle("GET /cal/", static)
	mux.Handle("GET /app/", static)
	mux.Handle("GET /shared/", static)
	mux.Handle("GET /{$}", http.RedirectHandler("/cal/", http.StatusFound))
	mux.Handle("/", proxy)

	slog.Info("calclient listening", "addr", *addr, "target", targetURL.String())
	if err := http.ListenAndServe(*addr, withAccessLog(mux)); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

// withAccessLog logs one line per request. calclient authenticates nothing
// itself — it proxies to calserver and even strips the challenge on 401s — so
// without this a wrong login was invisible on the client side. A proxied 401/403
// (a rejected login) logs at Warn so it shows at the default level; everything
// else logs at Debug to stay quiet for the static-asset traffic.
func withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)

		level, msg := slog.LevelDebug, "request"
		if rec.status == http.StatusUnauthorized || rec.status == http.StatusForbidden {
			level, msg = slog.LevelWarn, "login rejected"
		}
		slog.Log(r.Context(), level, msg,
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"remote", r.RemoteAddr, "dur", time.Since(start))
	})
}

// statusRecorder captures the response status for the access log while passing
// everything through. It preserves Flush so the reverse proxy can still stream
// responses (ReverseProxy flushes as it copies).
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	s.wrote = true // an implicit 200 if WriteHeader was never called
	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
