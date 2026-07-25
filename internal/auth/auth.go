// Package auth implements HTTP authentication for the DAV server: a plugin
// running in the beforeMethod phase plus Basic and Digest backends. Port of
// sabre's lib/DAV/Auth.
package auth

import (
	"net/http"
	"strings"

	"calserver/internal/dav"
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
			dav.SetPrincipal(r.Context(), principal)
			return true, nil
		}
		reasons = append(reasons, reason)
	}
	if !p.AutoRequireLogin {
		return true, nil
	}
	for _, b := range p.backends {
		b.Challenge(w.Header())
	}
	msg := "No authentication backends were configured on this server."
	if len(reasons) > 0 {
		msg = strings.Join(reasons, ", ")
	}
	return false, dav.ErrNotAuthenticated("%s", msg)
}
