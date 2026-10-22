package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidatePushResource(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://push.example/abc", true},
		{"https://[2606:4700::1]/abc", true},
		{"http://push.example/abc", false},
		{"https://user:pw@push.example/abc", false},
		{"https://127.0.0.1/abc", false},
		{"https://10.0.0.5/abc", false},
		{"https://169.254.169.254/latest/meta-data", false},
		{"https://100.64.0.1/abc", false},
		{"https://[::1]/abc", false},
		{"https://[::ffff:127.0.0.1]/abc", false},
		{"https://localhost/abc", false},
		{"https://svc.localhost/abc", false},
		{"https:///abc", false},
	} {
		if err := validatePushResource(tc.url); (err == nil) != tc.ok {
			t.Errorf("validatePushResource(%q) = %v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

// The delivery client must refuse an internal address even when the name
// passed registration (a hostname resolving to loopback, say), and must not
// follow a push service's redirect.
func TestPushClientRefusesInternalAddresses(t *testing.T) {
	hit := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), "POST", srv.URL, nil)
	if resp, err := pushClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("pushClient reached loopback %s", srv.URL)
	}
	if hit {
		t.Fatal("loopback server received the request")
	}
	if pushClient.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("pushClient follows redirects")
	}
}
