package carddav_test

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/carddav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
)

type xmlName = xml.Name

const card1 = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:card1\r\nFN:Alice Smith\r\nEMAIL;TYPE=WORK:alice@example.org\r\nEND:VCARD\r\n"
const card2 = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:card2\r\nFN:Bob Jones\r\nEMAIL;TYPE=HOME:bob@example.org\r\nEND:VCARD\r\n"

func buildServer() *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/admin"}}}
	backend := &carddav.MockBackend{
		AddressBooks: []carddav.AddressBookInfo{{
			ID: "1", URI: "book1", PrincipalURI: "principals/admin",
			Props: map[xmlName]any{{Space: "DAV:", Local: "displayname"}: "Contacts"},
		}},
		CardsByBook: map[string]map[string]*carddav.CardInfo{
			"1": {
				"card1.vcf": {URI: "card1.vcf", AddressBookID: "1", CardData: []byte(card1)},
				"card2.vcf": {URI: "card2.vcf", AddressBookID: "1", CardData: []byte(card2)},
			},
		},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(carddav.NewAddressBookRoot(principals, backend, "addressbooks"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/admin"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&carddav.Plugin{})
	return srv
}

func do(t *testing.T, srv *dav.Server, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestAddressbookDiscovery(t *testing.T) {
	srv := buildServer()
	body := do(t, srv, "PROPFIND", "/principals/admin/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav"><d:prop><c:addressbook-home-set/></d:prop></d:propfind>`,
		"Depth", "0").Body.String()
	if !strings.Contains(body, "/addressbooks/admin/") {
		t.Errorf("addressbook-home-set missing:\n%s", body)
	}

	body = do(t, srv, "PROPFIND", "/addressbooks/admin/book1/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:displayname/></d:prop></d:propfind>`,
		"Depth", "0").Body.String()
	if !strings.Contains(body, "addressbook") || !strings.Contains(body, "Contacts") {
		t.Errorf("address book resourcetype/displayname missing:\n%s", body)
	}
}

func TestAddressbookGet(t *testing.T) {
	w := do(t, buildServer(), "GET", "/addressbooks/admin/book1/card1.vcf", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Alice Smith") {
		t.Errorf("GET card = %d:\n%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/vcard") {
		t.Errorf("Content-Type = %q, want text/vcard", ct)
	}
}

func TestAddressbookQuery(t *testing.T) {
	srv := buildServer()
	// FN contains "alice" (case-insensitive) → only card1.
	query := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/><c:address-data/></d:prop>
	  <c:filter><c:prop-filter name="FN"><c:text-match match-type="contains">alice</c:text-match></c:prop-filter></c:filter>
	</c:addressbook-query>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", query, "Depth", "1", "Content-Type", "application/xml")
	if w.Code != 207 {
		t.Fatalf("query status = %d:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "card1.vcf") {
		t.Errorf("query missed the matching card:\n%s", body)
	}
	if strings.Contains(body, "card2.vcf") {
		t.Errorf("query wrongly matched Bob:\n%s", body)
	}
}

func TestAddressbookMultiget(t *testing.T) {
	srv := buildServer()
	mg := `<?xml version="1.0"?><c:addressbook-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/><c:address-data/></d:prop>
	  <d:href>/addressbooks/admin/book1/card2.vcf</d:href>
	</c:addressbook-multiget>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", mg, "Content-Type", "application/xml")
	if w.Code != 207 || !strings.Contains(w.Body.String(), "Bob Jones") {
		t.Errorf("multiget = %d:\n%s", w.Code, w.Body.String())
	}
}

func TestAddressbookParamFilter(t *testing.T) {
	srv := buildServer()
	// EMAIL with TYPE=WORK param → only card1.
	query := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/></d:prop>
	  <c:filter><c:prop-filter name="EMAIL"><c:param-filter name="TYPE"><c:text-match match-type="equals">WORK</c:text-match></c:param-filter></c:prop-filter></c:filter>
	</c:addressbook-query>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", query, "Depth", "1", "Content-Type", "application/xml")
	body := w.Body.String()
	if !strings.Contains(body, "card1.vcf") || strings.Contains(body, "card2.vcf") {
		t.Errorf("param-filter TYPE=WORK wrong:\n%s", body)
	}
}
