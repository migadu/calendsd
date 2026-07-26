package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBasicAuthGuardsEveryEndpoint verifies that when the admin mux is wrapped
// with BasicAuth (as cmd/calserver-admin/main.go does), no route — read page,
// JSON API, or write action — is reachable without valid credentials. A 401
// uniquely proves the auth wrapper blocked the request: the handlers
// themselves never return 401, so any other status would mean the request got
// past the guard.
func TestBasicAuthGuardsEveryEndpoint(t *testing.T) {
	creds := map[string]string{"root": bcryptHash(t, "s3cret")}
	h := BasicAuth(creds, testApp().Handler())

	type route struct{ method, path string }
	// Every pattern registered in App.Handler(), plus unregistered paths to
	// prove the wrapper sits in front of the whole mux (not per-route). Keep
	// this list in sync with Handler(): a new endpoint added there without a
	// line here still gets caught by the catch-all cases at the end.
	routes := []route{
		{"GET", "/"},
		{"GET", "/principals"},
		{"GET", "/principal"},
		{"GET", "/calendar"},
		{"GET", "/addressbook"},
		{"GET", "/objects"},
		{"GET", "/object"},
		{"GET", "/cards"},
		{"GET", "/card"},
		{"GET", "/scheduling"},
		{"GET", "/sync"},
		{"GET", "/simulate"},
		{"GET", "/doctor"},
		{"GET", "/api/dashboard"},
		{"GET", "/api/principals"},
		{"GET", "/api/principal"},
		{"GET", "/api/calendar"},
		{"GET", "/api/addressbook"},
		{"GET", "/api/objects"},
		{"GET", "/api/object"},
		{"GET", "/api/cards"},
		{"GET", "/api/card"},
		{"GET", "/api/scheduling"},
		{"GET", "/api/sync"},
		{"GET", "/api/simulate"},
		{"GET", "/api/doctor"},
		{"POST", "/api/check-credentials"},
		{"POST", "/api/create-user"},
		{"POST", "/api/set-password"},
		{"POST", "/api/set-delegation"},
		{"POST", "/api/proppatch"},
		{"POST", "/api/provision"},
		{"POST", "/api/delete-orphans"},
		// Unregistered paths: the wrapper must still block them, proving it
		// wraps the entire mux rather than individual handlers.
		{"GET", "/does-not-exist"},
		{"POST", "/api/not-a-real-endpoint"},
		{"DELETE", "/principal"},
	}

	status := func(method, path, user, pass string, withAuth bool) int {
		req := httptest.NewRequest(method, path, nil)
		if withAuth {
			req.SetBasicAuth(user, pass)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	for _, r := range routes {
		if code := status(r.method, r.path, "", "", false); code != http.StatusUnauthorized {
			t.Errorf("%s %s without credentials = %d, want 401", r.method, r.path, code)
		}
		if code := status(r.method, r.path, "root", "wrong", true); code != http.StatusUnauthorized {
			t.Errorf("%s %s with wrong password = %d, want 401", r.method, r.path, code)
		}
		if code := status(r.method, r.path, "nobody", "s3cret", true); code != http.StatusUnauthorized {
			t.Errorf("%s %s as unknown user = %d, want 401", r.method, r.path, code)
		}
	}

	// Sanity check the negative: valid credentials get PAST the wrapper, so the
	// blanket 401s above are the guard working, not a broken handler.
	if code := status("GET", "/", "root", "s3cret", true); code == http.StatusUnauthorized {
		t.Fatalf("valid credentials returned 401 on the dashboard; wrapper is over-blocking")
	}
}
