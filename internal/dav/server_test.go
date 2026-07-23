package dav

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOptions(t *testing.T) {
	s := NewServer("/")
	req := httptest.NewRequest("OPTIONS", "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Header().Get("DAV"), "1, 3, extended-mkcol"; got != want {
		t.Errorf("DAV header = %q, want %q", got, want)
	}
	allow := rec.Header().Get("Allow")
	for _, m := range []string{"OPTIONS", "PROPFIND", "PUT", "MKCOL", "PROPPATCH", "COPY", "MOVE"} {
		if !strings.Contains(allow, m) {
			t.Errorf("Allow header %q missing %s", allow, m)
		}
	}
	if got := rec.Header().Get("MS-Author-Via"); got != "DAV" {
		t.Errorf("MS-Author-Via = %q, want DAV", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("OPTIONS body should be empty, got %q", rec.Body.String())
	}
}

func TestUnimplementedMethod(t *testing.T) {
	s := NewServer("/")
	req := httptest.NewRequest("REPORT", "/calendars/admin/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestFeaturesAggregation(t *testing.T) {
	s := NewServer("/")
	s.AddPlugin(fakePlugin{features: []string{"calendar-access", "1"}})

	got := strings.Join(s.Features(), ", ")
	if want := "1, 3, extended-mkcol, calendar-access"; got != want {
		t.Errorf("Features() = %q, want %q (deduplicated, registration order)", got, want)
	}
}

type fakePlugin struct{ features []string }

func (f fakePlugin) PluginName() string { return "fake" }
func (f fakePlugin) Init(*Server)       {}
func (f fakePlugin) Features() []string { return f.features }

func TestPath(t *testing.T) {
	for _, tc := range []struct {
		base, uri, want string
		wantErr         bool
	}{
		{"/", "/", "", false},
		{"/", "/calendars/admin/", "calendars/admin", false},
		{"/dav/", "/dav/calendars", "calendars", false},
		{"/dav/", "/dav", "", false},            // request for the base itself, no trailing slash
		{"/dav/", "/other/calendars", "", true}, // outside base uri
		{"/", "/a%20b/c%2Fd", "a b/c/d", false}, // sabre decodes %2F to a literal slash post-split
	} {
		s := NewServer(tc.base)
		req := httptest.NewRequest("GET", tc.uri, nil)
		got, err := s.Path(req)
		if tc.wantErr != (err != nil) {
			t.Errorf("Path(base=%q, %q) error = %v, wantErr %v", tc.base, tc.uri, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("Path(base=%q, %q) = %q, want %q", tc.base, tc.uri, got, tc.want)
		}
	}
}
