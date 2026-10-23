package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWrapRequestDeadlineSetsContextDeadline(t *testing.T) {
	var saw time.Time
	var ok bool
	h := WrapRequestDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw, ok = r.Context().Deadline()
	}), 42*time.Second)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !ok {
		t.Fatal("handler context carries no deadline")
	}
	if remaining := time.Until(saw); remaining > 42*time.Second || remaining < 40*time.Second {
		t.Errorf("deadline %v from now, want about 42 s", remaining)
	}
	// Zero disables the wrapper rather than installing an already-expired context.
	ok = false
	WrapRequestDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok = r.Context().Deadline()
	}), 0).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if ok {
		t.Error("a zero deadline installed a deadline")
	}
}
