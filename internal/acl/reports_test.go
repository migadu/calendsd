package acl_test

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

type xmlName = xml.Name

func searchServer() *dav.Server {
	dn := xmlName{Space: "DAV:", Local: "displayname"}
	email := xmlName{Space: "http://sabredav.org/ns", Local: "email-address"}
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/alice", Props: map[xmlName]any{dn: "Alice Anderson", email: "alice@example.org"}},
		{URI: "principals/bob", Props: map[xmlName]any{dn: "Bob Brown", email: "bob@example.org"}},
		{URI: "principals/admin", Props: map[xmlName]any{dn: "Administrator"}},
	}}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/admin"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	return srv
}

func report(t *testing.T, srv *dav.Server, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("REPORT", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestPrincipalPropertySearch(t *testing.T) {
	srv := searchServer()
	body := `<?xml version="1.0"?>
	<d:principal-property-search xmlns:d="DAV:">
	  <d:property-search>
	    <d:prop><d:displayname/></d:prop>
	    <d:match>anderson</d:match>
	  </d:property-search>
	  <d:prop><d:displayname/></d:prop>
	</d:principal-property-search>`
	w := report(t, srv, "/principals/", body, "Depth", "0")
	if w.Code != 207 {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if !strings.Contains(out, "principals/alice") || !strings.Contains(out, "Alice Anderson") {
		t.Errorf("search missed Alice:\n%s", out)
	}
	if strings.Contains(out, "principals/bob") {
		t.Errorf("search wrongly matched Bob:\n%s", out)
	}
}

func TestPrincipalSearchPropertySet(t *testing.T) {
	w := report(t, searchServer(), "/principals/",
		`<?xml version="1.0"?><d:principal-search-property-set xmlns:d="DAV:"/>`, "Depth", "0")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var doc struct {
		XMLName    xmlName `xml:"DAV: principal-search-property-set"`
		Properties []struct {
			Prop struct {
				Names []struct{ XMLName xmlName } `xml:",any"`
			} `xml:"DAV: prop"`
			Description string `xml:"DAV: description"`
		} `xml:"DAV: principal-search-property"`
	}
	if err := xml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("search-property-set body: %v\n%s", err, w.Body.String())
	}
	if len(doc.Properties) == 0 || len(doc.Properties[0].Prop.Names) != 1 ||
		doc.Properties[0].Prop.Names[0].XMLName != (xmlName{Space: "DAV:", Local: "displayname"}) ||
		doc.Properties[0].Description != "Display name" {
		t.Errorf("search-property-set body wrong:\n%s", w.Body.String())
	}
}

func TestExpandProperty(t *testing.T) {
	// current-user-principal is an href; expanding it inlines the
	// principal's displayname without a second request.
	srv := searchServer()
	body := `<?xml version="1.0"?>
	<d:expand-property xmlns:d="DAV:">
	  <d:property name="current-user-principal">
	    <d:property name="displayname"/>
	  </d:property>
	</d:expand-property>`
	w := report(t, srv, "/principals/admin/", body)
	if w.Code != 207 {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	// The expanded current-user-principal should carry a nested response
	// with the admin's displayname.
	if !strings.Contains(out, "current-user-principal") || !strings.Contains(out, "Administrator") {
		t.Errorf("expand-property did not inline the principal:\n%s", out)
	}
}
