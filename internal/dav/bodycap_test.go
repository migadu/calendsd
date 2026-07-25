package dav

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestBodyCap verifies a request body over MaxRequestBodySize is
// rejected with 413 rather than being read fully into memory.
func TestRequestBodyCap(t *testing.T) {
	s := NewServer("/", testRoot())
	s.MaxRequestBodySize = 16

	big := strings.Repeat("x", 4096)
	req := httptest.NewRequest("PUT", "/big.txt", strings.NewReader(big))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize PUT = %d, want 413", rec.Code)
	}

	// A body within the limit must not be rejected for size.
	small := httptest.NewRequest("PUT", "/small.txt", strings.NewReader("tiny"))
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, small)
	if rec2.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("within-limit PUT wrongly rejected as 413")
	}
}
