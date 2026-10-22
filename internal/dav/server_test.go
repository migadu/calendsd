package dav

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// WriteError's body is well-formed XML whatever the message holds, and
// carries the preconditions in their own namespaces.
func TestWriteErrorBody(t *testing.T) {
	s := NewServer("/", testRoot())
	rec := httptest.NewRecorder()
	s.WriteError(rec, &Error{
		Status:       http.StatusForbidden,
		Exception:    `Sabre\DAV\Exception\Forbidden`,
		Message:      `a <b> & "c" ]]>`,
		Precondition: []xml.Name{{Space: "urn:ietf:params:xml:ns:caldav", Local: "valid-calendar-data"}},
	})
	var doc struct {
		XMLName      xml.Name  `xml:"DAV: error"`
		Precondition *struct{} `xml:"urn:ietf:params:xml:ns:caldav valid-calendar-data"`
		Exception    string    `xml:"http://sabredav.org/ns exception"`
		Message      string    `xml:"http://sabredav.org/ns message"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("error body is not XML: %v\n%s", err, rec.Body.String())
	}
	if doc.Precondition == nil || doc.Exception != `Sabre\DAV\Exception\Forbidden` || doc.Message != `a <b> & "c" ]]>` {
		t.Errorf("error body = %+v:\n%s", doc, rec.Body.String())
	}
}

// supported-method-set lists one supported-method element per allowed
// method.
func TestSupportedMethodSet(t *testing.T) {
	s := NewServer("/", testRoot())
	req := httptest.NewRequest("PROPFIND", "/", strings.NewReader(
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:supported-method-set/></d:prop></d:propfind>`))
	req.Header.Set("Depth", "0")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, `<d:supported-method name="PROPFIND"/>`) {
		t.Errorf("supported-method-set missing PROPFIND:\n%s", body)
	}
}

func TestOptions(t *testing.T) {
	s := NewServer("/", testRoot())
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
	// MKCOL is absent: sabre only advertises it on unmapped URIs, and "/"
	// exists.
	for _, m := range []string{"OPTIONS", "PROPFIND", "PUT", "PROPPATCH", "COPY", "MOVE", "REPORT"} {
		if !strings.Contains(allow, m) {
			t.Errorf("Allow header %q missing %s", allow, m)
		}
	}
	if strings.Contains(allow, "MKCOL") {
		t.Errorf("Allow header %q must not advertise MKCOL on an existing path", allow)
	}
	if got := rec.Header().Get("MS-Author-Via"); got != "DAV" {
		t.Errorf("MS-Author-Via = %q, want DAV", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("OPTIONS body should be empty, got %q", rec.Body.String())
	}
}

func TestUnimplementedMethod(t *testing.T) {
	s := NewServer("/", testRoot())
	req := httptest.NewRequest("BREW", "/calendars/admin/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestFeaturesAggregation(t *testing.T) {
	s := NewServer("/", testRoot())
	s.AddPlugin(fakePlugin{features: []string{"calendar-access", "1"}})

	got := strings.Join(s.Features(), ", ")
	if want := "1, 3, extended-mkcol, calendar-access"; got != want {
		t.Errorf("Features() = %q, want %q (deduplicated, registration order)", got, want)
	}
}

// testRoot is a minimal in-package root collection (the mock package can't
// be imported here without a cycle).
func testRoot() Collection { return emptyColl{} }

type emptyColl struct{}

func (emptyColl) Name() string                 { return "root" }
func (emptyColl) SetName(string) error         { return nil }
func (emptyColl) Delete() error                { return nil }
func (emptyColl) LastModified() time.Time      { return time.Time{} }
func (emptyColl) Children() ([]Node, error)    { return nil, nil }
func (emptyColl) ChildExists(string) bool      { return false }
func (emptyColl) CreateDirectory(string) error { return nil }
func (emptyColl) Child(name string) (Node, error) {
	return nil, ErrNotFound("File not found: %s", name)
}
func (emptyColl) CreateFile(string, io.Reader) (string, error) { return "", nil }

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
		{"/", "/a%20b/c", "a b/c", false},
		{"/", "/a%20b/c%2Fd", "", true}, // an encoded slash would split a segment after Clean
	} {
		s := NewServer(tc.base, testRoot())
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
