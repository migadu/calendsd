package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withCapturedLog installs a text slog logger writing to buf for the duration
// of fn, then restores the previous default.
func withCapturedLog(buf *bytes.Buffer, fn func()) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
}

func TestAccessLogDisabledIsPassthrough(t *testing.T) {
	var buf bytes.Buffer
	withCapturedLog(&buf, func() {
		h := WrapAccessLog(echoHandler(200, "ok"), false)
		if h == nil {
			t.Fatal("nil handler")
		}
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	})
	if buf.Len() != 0 {
		t.Fatalf("disabled access log still wrote: %q", buf.String())
	}
}

func TestAccessLogFields(t *testing.T) {
	var buf bytes.Buffer
	withCapturedLog(&buf, func() {
		h := WrapAccessLog(echoHandler(200, "hello"), true)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("REPORT", "/calendars/u/c/?x=1", nil))
	})
	got := buf.String()
	for _, want := range []string{
		"level=INFO", `msg="http request"`, "method=REPORT",
		`path="/calendars/u/c/?x=1"`, "status=200", "bytes=5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("access line %q missing %q", got, want)
		}
	}
}

func TestAccessLogLevelByStatus(t *testing.T) {
	cases := []struct {
		status int
		level  string
	}{
		{200, "level=INFO"},
		{207, "level=INFO"},
		{401, "level=INFO"}, // routine auth challenge, not a failure
		{403, "level=WARN"},
		{412, "level=WARN"}, // the move/put conflict operators want to see
		{500, "level=ERROR"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		withCapturedLog(&buf, func() {
			h := WrapAccessLog(echoHandler(tc.status, ""), true)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("MOVE", "/o.ics", nil))
		})
		if got := buf.String(); !strings.Contains(got, tc.level) {
			t.Errorf("status %d: want %s, got %q", tc.status, tc.level, got)
		}
	}
}

func TestAccessLogPanicLoggedAs500(t *testing.T) {
	var buf bytes.Buffer
	panicked := false
	withCapturedLog(&buf, func() {
		h := WrapAccessLog(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}), true)
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	})
	if !panicked {
		t.Fatal("panic was swallowed; net/http recovery semantics broken")
	}
	if got := buf.String(); !strings.Contains(got, "status=500") || !strings.Contains(got, "level=ERROR") {
		t.Errorf("panic not logged as 500 error: %q", got)
	}
}

// The contacts API's queries are what users type; its access log keeps the
// path and drops them.
func TestAccessLogPathOnlyDropsQuery(t *testing.T) {
	var buf bytes.Buffer
	withCapturedLog(&buf, func() {
		h := WrapAccessLogPathOnly(echoHandler(200, ""), true)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/suggestions?q=secret+typing", nil))
	})
	got := buf.String()
	if strings.Contains(got, "secret") || !strings.Contains(got, "path=/api/v1/suggestions ") {
		t.Errorf("path-only access log = %q", got)
	}
}

// The recorders sit between net/http and every handler, so they must let
// http.ResponseController reach the real writer (deadlines, full duplex).
func TestRecordersUnwrap(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("EnableFullDuplex through the wrappers: %v", err)
		}
	})
	h := WrapAccessLog(WrapRequestLog(inner, t.TempDir()), true)
	srv := httptest.NewServer(h)
	defer srv.Close()
	var buf bytes.Buffer
	withCapturedLog(&buf, func() {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	})
}
