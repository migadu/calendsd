// Package auth implements HTTP authentication for the DAV server: a plugin
// running in the beforeMethod phase plus Basic and Digest backends. Port of
// sabre's lib/DAV/Auth.
package auth

import (
	"log/slog"
	"net/http"
	"strings"

	"calserver/internal/dav"
	"calserver/internal/metrics"
)

// Backend verifies request credentials. Port of sabre's
// Auth\Backend\BackendInterface.
type Backend interface {
	// Check returns the authenticated principal path (e.g.
	// "principals/admin") or ok=false with a human-readable reason.
	Check(r *http.Request) (principal string, ok bool, reason string)
	// Challenge adds WWW-Authenticate headers for a 401 response.
	Challenge(h http.Header)
}

// Plugin authenticates every request before dispatch, recording the
// principal in the request context (sabre keeps it on the per-request plugin
// instance; a long-lived Go server must scope it per request). Port of
// sabre's Auth\Plugin.
type Plugin struct {
	backends []Backend

	// AutoRequireLogin rejects unauthenticated requests with a 401, like
	// sabre's autoRequireLogin.
	AutoRequireLogin bool

	// fixedPrincipal, when set, authenticates every request as that
	// principal (the autoLogin mechanism of sabre's test harness).
	fixedPrincipal string

	// Logger records authentication outcomes: rejected logins at Warn (with
	// the attempted username and client address, never the password) and
	// successful ones at Debug. Nil uses slog.Default(). sabre logged none of
	// this, so a brute-force or a misconfigured client was previously
	// invisible on the server side.
	Logger *slog.Logger

	// Metrics counts credential-less probes (backend="none",
	// outcome="no_credentials"); attempts that carry credentials are counted
	// by the backend that owns the credential type, so the outcome
	// classification lives next to the code that knows it. A nil Set records
	// nothing.
	Metrics *metrics.Set

	// Tombstoned, when non-nil, reports whether an otherwise-authenticated
	// principal has been soft-deleted (scheduled for deletion). Such requests
	// are rejected with 403 even though the credentials are valid, disabling
	// the account immediately without destroying its password. Nil disables
	// the check — parity with the pre-soft-delete server (SOFT-DELETE-PLAN.md
	// §5). It runs above the backends, so it covers every auth mechanism.
	Tombstoned func(principal string) bool
}

func (p *Plugin) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// NewPlugin creates the auth plugin with the given backends.
func NewPlugin(backends ...Backend) *Plugin {
	return &Plugin{backends: backends, AutoRequireLogin: true}
}

// NewLoggedInPlugin creates a pre-authenticated plugin, mirroring the
// autoLogin mechanism of sabre's test harness. An empty principal would
// otherwise fall through to an unauthenticated, require-login-disabled state
// (an accidental open server), so it panics instead.
func NewLoggedInPlugin(principal string) *Plugin {
	if principal == "" {
		panic("auth.NewLoggedInPlugin: principal must not be empty")
	}
	return &Plugin{fixedPrincipal: principal, AutoRequireLogin: true}
}

func (p *Plugin) PluginName() string { return "auth" }

func (p *Plugin) Init(s *dav.Server) {}

// BeforeMethod implements dav.BeforeMethodHook.
func (p *Plugin) BeforeMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if p.fixedPrincipal != "" {
		dav.SetPrincipal(r.Context(), p.fixedPrincipal)
		return true, nil
	}
	var reasons []string
	for _, b := range p.backends {
		principal, ok, reason := b.Check(r)
		if ok {
			if p.Tombstoned != nil && p.Tombstoned(principal) {
				// Valid credentials, but the account is scheduled for deletion:
				// reject with 403 (not a 401 re-challenge, which would loop) and
				// never set the principal, so nothing downstream runs as them.
				p.logger().Warn("auth blocked: account scheduled for deletion",
					"principal", principal, "method", r.Method,
					"path", r.URL.Path, "remote", remoteAddr(r))
				p.Metrics.AuthAttempt("tombstoned", "blocked")
				return false, dav.ErrForbidden("this account is scheduled for deletion")
			}
			dav.SetPrincipal(r.Context(), principal)
			p.logger().Debug("auth ok",
				"principal", principal, "method", r.Method,
				"path", r.URL.Path, "remote", remoteAddr(r))
			return true, nil
		}
		reasons = append(reasons, reason)
	}
	if !p.AutoRequireLogin {
		return true, nil
	}
	// A rejected login was previously silent; record it (never the password)
	// so wrong logins are visible at the default level. A request with no
	// credentials at all is the ordinary pre-challenge probe every DAV client
	// makes, so log that at Debug and only a real credential rejection at Warn.
	user, presented := attemptedLogin(r)
	level := slog.LevelDebug
	if presented {
		level = slog.LevelWarn
		if !credentialParsed(r) {
			// An Authorization header no backend could parse (Bearer, mangled
			// base64 Basic): no backend counted it, so count it here — a
			// rejection must never vanish from the attempts series.
			p.Metrics.AuthAttempt("none", "error")
		}
	} else {
		p.Metrics.AuthAttempt("none", "no_credentials")
	}
	p.logger().Log(r.Context(), level, "auth failed",
		"user", user, "method", r.Method, "path", r.URL.Path,
		"remote", remoteAddr(r), "reason", strings.Join(reasons, "; "))

	for _, b := range p.backends {
		b.Challenge(w.Header())
	}
	msg := "No authentication backends were configured on this server."
	if len(reasons) > 0 {
		msg = strings.Join(reasons, ", ")
	}
	return false, dav.ErrNotAuthenticated("%s", msg)
}

// attemptedLogin best-effort extracts the username a client presented, so a
// rejected login names who tried. presented reports whether the request
// carried any credentials at all: without it, the log line is just the routine
// unauthenticated probe that precedes every challenge, not a failed attempt.
func attemptedLogin(r *http.Request) (user string, presented bool) {
	if u, _, ok := r.BasicAuth(); ok {
		return u, true
	}
	if params := parseDigestHeader(r.Header.Get("Authorization")); params != nil {
		return params["username"], true
	}
	// Any other non-empty Authorization header (Bearer, base64 that doesn't
	// decode) still carries a credential: its rejection must log at Warn and
	// count as an attempt, not read as a routine pre-challenge probe.
	return "", r.Header.Get("Authorization") != ""
}

// credentialParsed reports whether the Authorization header parsed as one of
// the credential types the backends count themselves (Basic or Digest).
func credentialParsed(r *http.Request) bool {
	if _, _, ok := r.BasicAuth(); ok {
		return true
	}
	return parseDigestHeader(r.Header.Get("Authorization")) != nil
}

// remoteAddr reports the client address for a log line. When the request came
// through the calclient reverse proxy, r.RemoteAddr is the proxy's loopback
// address, so surface the proxy-supplied X-Forwarded-For too — untrusted (a
// client can forge it when it reaches calserver directly), but the only way to
// see the real client behind the proxy.
func remoteAddr(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff + " (via " + r.RemoteAddr + ")"
	}
	return r.RemoteAddr
}
