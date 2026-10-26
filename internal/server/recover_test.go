package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoverAnswers500(t *testing.T) {
	var buf bytes.Buffer
	rr := httptest.NewRecorder()
	withCapturedLog(&buf, func() {
		h := WrapAccessLog(WrapRecover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"x"`)
			panic("boom")
		})), true)
		h.ServeHTTP(rr, httptest.NewRequest("PUT", "/calendars/u/c/e.ics", nil))
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if rr.Header().Get("ETag") != "" {
		t.Errorf("staged ETag leaked onto the 500")
	}
	got := buf.String()
	for _, want := range []string{`msg="http handler panic"`, "panic=boom", "stack=", "status=500"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q missing %q", got, want)
		}
	}
}

func TestRecoverAfterWriteAborts(t *testing.T) {
	h := WrapRecover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		panic("boom")
	}))
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
		}
	}()
	var buf bytes.Buffer
	withCapturedLog(&buf, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("REPORT", "/", nil))
	})
}

func TestRecoverPassesAbortHandler(t *testing.T) {
	h := WrapRecover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestRecoverJSONBody(t *testing.T) {
	var buf bytes.Buffer
	rr := httptest.NewRecorder()
	withCapturedLog(&buf, func() {
		WrapRecoverJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		})).ServeHTTP(rr, httptest.NewRequest("GET", "/api/contacts", nil))
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	if got := rr.Body.String(); got != "{\"error\":\"internal error\"}\n" {
		t.Errorf("body = %q", got)
	}
}
