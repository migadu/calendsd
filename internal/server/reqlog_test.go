package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// echoHandler reads the whole request body (so the capturing tee is exercised)
// and writes a fixed response.
func echoHandler(status int, respBody string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	})
}

func readOnlyLog(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d log files, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return string(data)
}

func TestRequestLogCapturesBothDirections(t *testing.T) {
	dir := t.TempDir()
	h := WrapRequestLog(echoHandler(207, "<multistatus/>"), dir)

	req := httptest.NewRequest("REPORT", "/calendars/user1/cal", strings.NewReader("<query/>"))
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The handler's response must reach the client unchanged.
	if rec.Code != 207 || rec.Body.String() != "<multistatus/>" {
		t.Fatalf("response = %d %q, want 207 <multistatus/>", rec.Code, rec.Body.String())
	}

	log := readOnlyLog(t, dir)
	for _, want := range []string{
		"Method:   REPORT",
		"Path:     /calendars/user1/cal",
		"Depth: 1",
		"<query/>",         // request body captured
		"Status: 207",      // response status captured
		"<multistatus/>",   // response body captured
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q\n---\n%s", want, log)
		}
	}
}

func TestRequestLogRedactsAuthorization(t *testing.T) {
	dir := t.TempDir()
	h := WrapRequestLog(echoHandler(200, "ok"), dir)

	req := httptest.NewRequest("GET", "/", nil)
	req.SetBasicAuth("user@example.com", "s3cret-password")
	h.ServeHTTP(httptest.NewRecorder(), req)

	log := readOnlyLog(t, dir)
	if strings.Contains(log, "s3cret-password") {
		t.Error("log leaked the Basic password")
	}
	// The base64 of user:pass must not appear either.
	if strings.Contains(log, req.Header.Get("Authorization")[len("Basic "):]) {
		t.Error("log leaked the base64 Authorization value")
	}
	if !strings.Contains(log, "Authorization: [redacted]") {
		t.Errorf("Authorization not redacted\n---\n%s", log)
	}
}

func TestRequestLogDisabledByEmptyDir(t *testing.T) {
	inner := echoHandler(200, "ok")
	if got := WrapRequestLog(inner, ""); got == nil {
		t.Fatal("WrapRequestLog(\"\") returned nil; want the handler unchanged")
	}
	// Nothing to assert on files; just confirm it serves.
	rec := httptest.NewRecorder()
	WrapRequestLog(inner, "").ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

func TestRequestLogTruncatesLargeBody(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("A", maxLoggedBody+4096)
	h := WrapRequestLog(echoHandler(200, big), dir)

	req := httptest.NewRequest("PUT", "/calendars/user1/cal/x.ics", strings.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Client still gets the full response despite the log cap.
	if len(rec.Body.String()) != len(big) {
		t.Fatalf("client got %d bytes, want %d (logging must not truncate the wire)", rec.Body.Len(), len(big))
	}
	log := readOnlyLog(t, dir)
	if !strings.Contains(log, "truncated") {
		t.Errorf("large bodies should be marked truncated in the log")
	}
}
