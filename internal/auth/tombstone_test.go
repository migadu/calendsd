package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"calserver/internal/dav"
	"calserver/internal/metrics"
)

// stubBackend authenticates every request as a fixed principal when ok.
type stubBackend struct {
	principal string
	ok        bool
}

func (s stubBackend) Check(*http.Request) (string, bool, string) {
	if s.ok {
		return s.principal, true, ""
	}
	return "", false, "no"
}
func (stubBackend) Challenge(http.Header, *http.Request) {}

// TestPluginTombstoneGate: a soft-deleted account with valid credentials is
// rejected with 403 and counted as blocked, while a live account and a nil
// gate pass through unchanged.
func TestPluginTombstoneGate(t *testing.T) {
	set := metrics.New("test")
	p := NewPlugin(stubBackend{principal: "principals/alice", ok: true})
	p.Metrics = set

	// Tombstoned: correct creds, but 403 (not a 401 re-challenge) and no proceed.
	p.Tombstoned = func(principal string) bool { return principal == "principals/alice" }
	proceed, err := p.BeforeMethod(httptest.NewRecorder(), httptest.NewRequest("PROPFIND", "/", nil))
	if proceed {
		t.Fatal("tombstoned account was allowed through")
	}
	var de *dav.Error
	if !errors.As(err, &de) || de.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 dav.Error", err)
	}
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(attemptsWant(
		`calserver_auth_attempts_total{backend="tombstoned",outcome="blocked"} 1`,
	)), "calserver_auth_attempts_total"); err != nil {
		t.Error(err)
	}

	// Live account (gate returns false): the same credentials pass.
	p.Tombstoned = func(string) bool { return false }
	if proceed, err := p.BeforeMethod(httptest.NewRecorder(), httptest.NewRequest("PROPFIND", "/", nil)); !proceed || err != nil {
		t.Fatalf("live account rejected: proceed=%v err=%v", proceed, err)
	}

	// Nil gate: parity with the pre-soft-delete server.
	p.Tombstoned = nil
	if proceed, err := p.BeforeMethod(httptest.NewRecorder(), httptest.NewRequest("PROPFIND", "/", nil)); !proceed || err != nil {
		t.Fatalf("nil gate rejected: proceed=%v err=%v", proceed, err)
	}
}
