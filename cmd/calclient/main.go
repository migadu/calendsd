// calclient serves the demo web client and reverse-proxies DAV requests to a
// calendsd, so the browser talks to one origin and CORS never comes into
// play (CalDAV needs PROPFIND/REPORT/MKCALENDAR/PROPPATCH/MOVE, which a
// cross-origin page cannot send without preflight support the server does
// not offer).
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"time"

	"calendsd/internal/logging"
	"calendsd/internal/version"
)

// Only the three served asset trees — never `web` wholesale, which would pull
// web/e2e (Playwright tests and their node_modules) into the binary.
//
//go:embed web/app web/cal web/shared
var webFS embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8091", "listen address for the client UI and proxy")
	target := flag.String("target", "http://127.0.0.1:8080", "base URL of the calendsd to proxy DAV requests to")
	devAssets := flag.String("dev-assets", "", "serve client assets from this directory instead of the embedded copy (for iterating without rebuilds)")
	logLevel := flag.String("log-level", "", "log verbosity: debug, info, warn, or error (default info). When empty, $CALSERVER_LOG_LEVEL applies")
	showVersion := version.Flags()
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("calclient"))
		return
	}

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
	//
	// no-cache: embedded assets carry no ETag/Last-Modified (embed.FS has no
	// mod times), so without an explicit directive a browser may keep running
	// JS from a previous build after a rebuild + restart — old client code
	// against a new server, the "still broken after the fix" trap.
	static := noCacheAssets(http.FileServer(assets))
	// The end-user app owns real URL paths (/cal/calendar/week/…, /cal/event/…),
	// so any /cal/ path that isn't an actual asset file must serve the SPA shell
	// index.html and let the client router take over. /app/ and /shared/ have no
	// client routing, so they stay plain file servers.
	mux.Handle("GET /cal/", noCacheAssets(spaFallback(assets, http.FileServer(assets))))
	mux.Handle("GET /app/", static)
	mux.Handle("GET /shared/", static)
	mux.Handle("GET /{$}", http.RedirectHandler("/cal/", http.StatusFound))
	mux.Handle("/", proxy)

	ver, commit, built := version.Info()
	slog.Info("calclient listening", "addr", *addr, "target", targetURL.String(),
		"version", ver, "commit", commit, "built", built)
	// Only header and idle timeouts: the proxied DAV bodies and responses can
	// be large or slow, so a whole-request deadline would cut them off, but a
	// client that never finishes its headers or parks an idle connection
	// should not hold a goroutine forever.
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           withAccessLog(mux),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

// spaFallback serves real asset files as-is and rewrites every other /cal/ path
// to /cal/index.html, so the client's pushState router owns URLs like
// /cal/calendar/week/2026-08-13 and /cal/event/<uid>/edit without the file
// server 404ing on them. A request whose path names an existing (non-directory)
// file goes straight to the file server; anything else — a router path, or a
// bare directory — is served the SPA shell. Existence is checked through the
// http.FileSystem so it works identically for the embedded assets and the
// -dev-assets directory.
func spaFallback(assets http.FileSystem, fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, err := assets.Open(path.Clean(r.URL.Path)); err == nil {
			info, statErr := f.Stat()
			f.Close()
			if statErr == nil && !info.IsDir() {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// Serve the shell directly with ServeContent rather than rewriting to
		// /cal/index.html through the FileServer: the FileServer 301-redirects any
		// */index.html to its directory, which would bounce straight back here and
		// loop. ServeContent just writes the bytes (and sets text/html from the
		// name), so router paths and the bare /cal/ directory both get the shell.
		idx, err := assets.Open("/cal/index.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer idx.Close()
		info, err := idx.Stat()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, "index.html", info.ModTime(), idx)
	})
}

// noCacheAssets forces revalidation of the static client assets on every use
// (see the mux comment above).
func noCacheAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// withAccessLog logs one line per request. calclient authenticates nothing
// itself — it proxies to calendsd and even strips the challenge on 401s — so
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
