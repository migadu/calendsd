package auth

import "testing"

// TestNewLoggedInPluginRejectsEmpty ensures the pre-authenticated plugin
// refuses an empty principal, which would otherwise fall through to an
// unauthenticated, require-login-disabled (open-server) state.
func TestNewLoggedInPluginRejectsEmpty(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal(`NewLoggedInPlugin("") should panic`)
		}
	}()
	NewLoggedInPlugin("")
}

// TestNewLoggedInPluginRequiresLogin ensures a valid pre-authenticated plugin
// still rejects unauthenticated requests (AutoRequireLogin on).
func TestNewLoggedInPluginRequiresLogin(t *testing.T) {
	p := NewLoggedInPlugin("principals/admin")
	if !p.AutoRequireLogin {
		t.Error("AutoRequireLogin should be true so non-fixed paths still challenge")
	}
}
