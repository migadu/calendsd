// Package auth implements HTTP authentication for the DAV server: a plugin
// running in the beforeMethod phase plus Basic and Digest backends. Port of
// sabre's lib/DAV/Auth.
package auth

import (
	"net/http"

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

// Plugin authenticates every request before dispatch. Port of sabre's
// Auth\Plugin.
type Plugin struct {
	backends []Backend

	// AutoRequireLogin rejects unauthenticated requests with a 401, like
	// sabre's autoRequireLogin.
	AutoRequireLogin bool

	currentPrincipal string
}

// NewPlugin creates the auth plugin with the given backends.
func NewPlugin(backends ...Backend) *Plugin {
	return &Plugin{backends: backends, AutoRequireLogin: true}
}

// NewLoggedInPlugin creates a pre-authenticated plugin, mirroring the
// autoLogin mechanism of sabre's test harness.
func NewLoggedInPlugin(principal string) *Plugin {
	return &Plugin{currentPrincipal: principal}
}

func (p *Plugin) PluginName() string { return "auth" }

func (p *Plugin) Init(s *dav.Server) {}

// CurrentPrincipal returns the authenticated principal path, or "".
func (p *Plugin) CurrentPrincipal() string { return p.currentPrincipal }

// BeforeMethod implements dav.BeforeMethodHook.
func (p *Plugin) BeforeMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if p.currentPrincipal != "" {
		return true, nil
	}
	var reasons []string
	for _, b := range p.backends {
		principal, ok, reason := b.Check(r)
		if ok {
			p.currentPrincipal = principal
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
		msg = ""
		for i, reason := range reasons {
			if i > 0 {
				msg += ", "
			}
			msg += reason
		}
	}
	return false, dav.ErrNotAuthenticated("%s", msg)
}
