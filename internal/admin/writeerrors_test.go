package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/migadu/calendsd/internal/storage"
)

type mappedWrites struct {
	fakeWrites
	err error
}

func (m *mappedWrites) SetPassword(context.Context, string, string) error { return m.err }
func (m *mappedWrites) DeleteUser(context.Context, string) error          { return m.err }
func (m *mappedWrites) CreateUser(context.Context, string, string, string, string) error {
	return m.err
}

// Storage errors map to 404 (no principal), 409 with text (an operator
// guard) or a generic 500 (audit 6.17).
func TestWriteErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
		body string
	}{
		{fmt.Errorf("no users row for %q: %w", "x", storage.ErrNoPrincipal), http.StatusNotFound, "no such user"},
		{fmt.Errorf("x still holds 2 calendar instance(s): %w", storage.ErrHoldsCollections), http.StatusConflict, "still holds"},
		{fmt.Errorf("user %q: %w", "x", storage.ErrAlreadyExists), http.StatusConflict, "already exists"},
		{fmt.Errorf("invalid username %q: %w", "a/b", storage.ErrInvalidUsername), http.StatusConflict, "invalid username"},
		{errors.New("pq: connection refused"), http.StatusInternalServerError, "see the server log"},
		// A failed INSERT that is not a duplicate is an internal error even
		// though it comes from the same code path as the duplicate case.
		{errors.New("creating users row: pq: connection refused (host=db.internal)"), http.StatusInternalServerError, "see the server log"},
	} {
		for _, path := range []string{"/api/set-password", "/api/delete-user", "/api/create-user"} {
			a := testAppB()
			a.Writes = &mappedWrites{err: tc.err}
			a.ReadOnly = false
			w := postForm(t, a.Handler(), path, url.Values{"username": {"x"}, "password": {"y"}}, true)
			if w.Code != tc.want {
				t.Errorf("%s with %q = %d, want %d", path, tc.err, w.Code, tc.want)
			}
			if body := w.Body.String(); !containsFold(body, tc.body) {
				t.Errorf("%s with %q: body %q should contain %q", path, tc.err, body, tc.body)
			}
			if tc.want == http.StatusInternalServerError && containsFold(w.Body.String(), "pq:") {
				t.Errorf("%s: driver text leaked to the client: %s", path, w.Body.String())
			}
		}
	}
}

func containsFold(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}
