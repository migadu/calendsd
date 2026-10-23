package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrapHealth(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	up := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("dial tcp: connection refused") }

	tests := []struct {
		name, method, path string
		ping               func(context.Context) error
		want               int
		body               string
	}{
		{"db up", "GET", "/health", up, 200, `{"database":"ok","status":"ok"}`},
		{"db down", "GET", "/health", down, 503, `{"database":"unreachable","status":"unavailable"}`},
		{"no db configured", "GET", "/health", nil, 200, `{"status":"ok"}`},
		{"head has no body", "HEAD", "/health", up, 200, ``},
		{"head db down", "HEAD", "/health", down, 503, ``},
		{"other method", "POST", "/health", up, 405, "method not allowed"},
		{"other path passes through", "GET", "/healthz", up, 418, ``},
		{"subpath passes through", "GET", "/health/x", up, 418, ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WrapHealth(inner, tt.ping).ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
			if got := strings.TrimSpace(w.Body.String()); got != tt.body {
				t.Errorf("body = %q, want %q", got, tt.body)
			}
		})
	}
}

func TestWrapHealthHidesDBError(t *testing.T) {
	w := httptest.NewRecorder()
	h := WrapHealth(http.NotFoundHandler(), func(context.Context) error {
		return errors.New("password authentication failed for user calserver")
	})
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if strings.Contains(w.Body.String(), "password") {
		t.Errorf("health body leaks the database error: %s", w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
