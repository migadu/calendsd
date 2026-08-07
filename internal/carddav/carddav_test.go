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

// TestCardPutValidation covers the write-time vCard validation
// (internal/carddav/plugin.go, InterceptWrite) that mirrors sabre's
// CardDAV\Plugin::validateVCard: a body that does not parse as a vCard is
// rejected with 415 instead of being stored raw, while a valid vCard is
// accepted.
func TestCardPutValidation(t *testing.T) {
	srv := buildServer()

	// Non-vCard garbage → 415 Unsupported Media Type, like sabre.
	w := do(t, srv, "PUT", "/addressbooks/admin/book1/bad.vcf",
		"this is not a vcard at all", "Content-Type", "text/vcard")
	if w.Code != 415 {
		t.Errorf("PUT of non-vCard = %d, want 415:\n%s", w.Code, w.Body.String())
	}

	// A real vCard is accepted.
	good := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:new1\r\nFN:New Person\r\nEND:VCARD\r\n"
	w = do(t, srv, "PUT", "/addressbooks/admin/book1/new1.vcf", good, "Content-Type", "text/vcard")
	if w.Code != 201 && w.Code != 204 {
		t.Errorf("PUT of valid vCard = %d, want 201/204:\n%s", w.Code, w.Body.String())
	}
}

// TestAddressbookQueryLimit covers <card:limit><card:nresults> truncation
// (internal/carddav/plugin.go / query.go), matching sabre's
// addressbookQueryReport limit break.
func TestAddressbookQueryLimit(t *testing.T) {
	srv := buildServerWithCards(map[string]*carddav.CardInfo{
		"card1.vcf": {URI: "card1.vcf", AddressBookID: "1", CardData: []byte(card1)},
		"card2.vcf": {URI: "card2.vcf", AddressBookID: "1", CardData: []byte(card2)},
	})
	// No filter → both cards match, but nresults=1 caps the result set at one.
	query := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/></d:prop>
	  <c:filter/>
	  <c:limit><c:nresults>1</c:nresults></c:limit>
	</c:addressbook-query>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", query, "Depth", "1", "Content-Type", "application/xml")
	if w.Code != 207 {
		t.Fatalf("query status = %d:\n%s", w.Code, w.Body.String())
	}
	if n := strings.Count(w.Body.String(), "<d:response"); n != 1 {
		t.Errorf("limit nresults=1 returned %d responses, want 1:\n%s", n, w.Body.String())
	}
}

// TestAddressbookQueryUnknownCollation covers parse-time rejection of an
// unsupported collation (internal/carddav/query.go, convertTextMatch): sabre's
// StringUtil::textMatch throws BadRequest, so the report is a 400 rather than a
// silently-defaulted 207.
func TestAddressbookQueryUnknownCollation(t *testing.T) {
	srv := buildServer()
	query := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/></d:prop>
	  <c:filter><c:prop-filter name="FN"><c:text-match collation="i;made-up">alice</c:text-match></c:prop-filter></c:filter>
	</c:addressbook-query>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", query, "Depth", "1", "Content-Type", "application/xml")
	if w.Code != 400 {
		t.Errorf("unknown collation = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

// TestAddressbookQueryAsciiCasemap covers the collation fold divergence
// (internal/carddav/query.go, foldCollation): i;ascii-casemap folds only ASCII
// a-z, so a non-ASCII letter is compared case-sensitively — matching sabre's
// StringUtil, and unlike i;unicode-casemap which folds it.
func TestAddressbookQueryAsciiCasemap(t *testing.T) {
	const cardAccent = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:acc\r\nFN:CAFÉ\r\nEND:VCARD\r\n"
	cards := map[string]*carddav.CardInfo{
		"cafe.vcf": {URI: "cafe.vcf", AddressBookID: "1", CardData: []byte(cardAccent)},
	}
	q := func(collation string) string {
		return `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
		  <d:prop><d:getetag/></d:prop>
		  <c:filter><c:prop-filter name="FN"><c:text-match collation="` + collation + `" match-type="equals">café</c:text-match></c:prop-filter></c:filter>
		</c:addressbook-query>`
	}

	// unicode-casemap folds É↔é, so "café" equals "CAFÉ" → match.
	w := do(t, buildServerWithCards(cards), "REPORT", "/addressbooks/admin/book1/",
		q("i;unicode-casemap"), "Depth", "1", "Content-Type", "application/xml")
	if !strings.Contains(w.Body.String(), "cafe.vcf") {
		t.Errorf("unicode-casemap should match CAFÉ:\n%s", w.Body.String())
	}

	// ascii-casemap folds only a-z, so É stays uppercase and é stays lowercase
	// → no match, exactly as sabre computes it.
	w = do(t, buildServerWithCards(cards), "REPORT", "/addressbooks/admin/book1/",
		q("i;ascii-casemap"), "Depth", "1", "Content-Type", "application/xml")
	if strings.Contains(w.Body.String(), "cafe.vcf") {
		t.Errorf("ascii-casemap must NOT fold É, so café != CAFÉ:\n%s", w.Body.String())
	}
}

// TestSupportedCollationSet covers the {CARDDAV:}supported-collation-set
// property (internal/carddav/plugin.go) sabre reports on address books.
func TestSupportedCollationSet(t *testing.T) {
	srv := buildServer()
	w := do(t, srv, "PROPFIND", "/addressbooks/admin/book1/",
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav"><d:prop><c:supported-collation-set/></d:prop></d:propfind>`,
		"Depth", "0")
	body := w.Body.String()
	for _, c := range []string{"i;ascii-casemap", "i;octet", "i;unicode-casemap"} {
		if !strings.Contains(body, c) {
			t.Errorf("supported-collation-set missing %q:\n%s", c, body)
		}
	}
}

// richCard has several distinguishable properties so address-data property
// filtering and version conversion can be observed.
const richCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:rich\r\nFN:Rich Person\r\nN:Person;Rich;;;\r\nEMAIL:rich@example.org\r\nTEL:+15551234\r\nEND:VCARD\r\n"

// TestAddressDataPartialRetrieval covers <card:address-data><card:prop
// name="…"/> partial retrieval in addressbook-query (internal/carddav
// convertCardData): only the requested properties plus the forced
// UID/VERSION/FN are returned, matching sabre's convertVCard property filter.
func TestAddressDataPartialRetrieval(t *testing.T) {
	srv := buildServerWithCards(map[string]*carddav.CardInfo{
		"rich.vcf": {URI: "rich.vcf", AddressBookID: "1", CardData: []byte(richCard)},
	})
	query := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/><c:address-data><c:prop name="EMAIL"/></c:address-data></d:prop>
	  <c:filter/>
	</c:addressbook-query>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", query, "Depth", "1", "Content-Type", "application/xml")
	if w.Code != 207 {
		t.Fatalf("query status = %d:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Requested EMAIL plus the forced UID/VERSION/FN are kept.
	for _, want := range []string{"rich@example.org", "UID:rich", "Rich Person", "VERSION:3.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("partial address-data missing %q:\n%s", want, body)
		}
	}
	// Non-requested, non-forced properties are stripped.
	if strings.Contains(body, "+15551234") {
		t.Errorf("partial address-data should have stripped TEL:\n%s", body)
	}
}

// TestAddressDataVersionConversion covers version negotiation
// (internal/carddav convertCardData): a stored 3.0 card requested as 4.0 is
// upgraded, matching sabre's convertVCard version target.
func TestAddressDataVersionConversion(t *testing.T) {
	srv := buildServerWithCards(map[string]*carddav.CardInfo{
		"rich.vcf": {URI: "rich.vcf", AddressBookID: "1", CardData: []byte(richCard)},
	})
	query := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/><c:address-data version="4.0"/></d:prop>
	  <c:filter/>
	</c:addressbook-query>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", query, "Depth", "1", "Content-Type", "application/xml")
	body := w.Body.String()
	if !strings.Contains(body, "VERSION:4.0") {
		t.Errorf("address-data version=4.0 should upgrade the stored 3.0 card:\n%s", body)
	}
	if strings.Contains(body, "VERSION:3.0") {
		t.Errorf("upgraded card should not still carry VERSION:3.0:\n%s", body)
	}
}

// TestAddressDataMultigetIgnoresPropFilter locks the sabre quirk that
// addressbook-multiget negotiates only the version and ignores nested
// <prop> filters (unlike addressbook-query): the full card comes back.
func TestAddressDataMultigetIgnoresPropFilter(t *testing.T) {
	srv := buildServerWithCards(map[string]*carddav.CardInfo{
		"rich.vcf": {URI: "rich.vcf", AddressBookID: "1", CardData: []byte(richCard)},
	})
	mg := `<?xml version="1.0"?><c:addressbook-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/><c:address-data><c:prop name="EMAIL"/></c:address-data></d:prop>
	  <d:href>/addressbooks/admin/book1/rich.vcf</d:href>
	</c:addressbook-multiget>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", mg, "Content-Type", "application/xml")
	body := w.Body.String()
	// The <prop name="EMAIL"/> filter must be ignored, so TEL survives.
	if !strings.Contains(body, "+15551234") {
		t.Errorf("multiget must ignore the address-data prop filter and return the whole card:\n%s", body)
	}
}

// photoB64 is a short stand-in for an embedded contact photo blob.
const photoB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg"
const photoCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:pic\r\nFN:Pic Person\r\nEMAIL:pic@example.org\r\nPHOTO;ENCODING=b;TYPE=PNG:" + photoB64 + "\r\nEND:VCARD\r\n"

// TestAddressDataPreservesPhoto documents that the CardDAV sync carries a
// contact's embedded PHOTO through end-to-end: an unfiltered fetch returns the
// blob untouched (convertCardData's raw passthrough), and an explicit
// address-data prop filter can either keep or drop it. The web client's fast
// list pass (contacts.js LIST_PROPS) deliberately omits PHOTO to skip the blob;
// this test locks in that the server honors both requests.
func TestAddressDataPreservesPhoto(t *testing.T) {
	newSrv := func() *dav.Server {
		return buildServerWithCards(map[string]*carddav.CardInfo{
			"pic.vcf": {URI: "pic.vcf", AddressBookID: "1", CardData: []byte(photoCard)},
		})
	}
	report := func(addressData string) string {
		query := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
		  <d:prop><d:getetag/>` + addressData + `</d:prop>
		  <c:filter/>
		</c:addressbook-query>`
		w := do(t, newSrv(), "REPORT", "/addressbooks/admin/book1/", query, "Depth", "1", "Content-Type", "application/xml")
		if w.Code != 207 {
			t.Fatalf("query status = %d:\n%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	// Whole card: convertCardData returns the stored bytes untouched, so the
	// PHOTO property and its exact base64 payload survive.
	whole := report(`<c:address-data/>`)
	if !strings.Contains(whole, "PHOTO") || !strings.Contains(whole, photoB64) {
		t.Errorf("unfiltered fetch dropped the embedded PHOTO:\n%s", whole)
	}

	// Fast list pass (EMAIL only, PHOTO not requested): the blob is stripped.
	light := report(`<c:address-data><c:prop name="EMAIL"/></c:address-data>`)
	if strings.Contains(light, "PHOTO") {
		t.Errorf("light list pass should omit the PHOTO blob:\n%s", light)
	}

	// Explicitly requesting PHOTO keeps it.
	withPhoto := report(`<c:address-data><c:prop name="PHOTO"/></c:address-data>`)
	if !strings.Contains(withPhoto, "PHOTO") || !strings.Contains(withPhoto, photoB64) {
		t.Errorf("address-data prop=PHOTO should return the blob:\n%s", withPhoto)
	}
}

// buildServerWithCards is buildServer with a custom card set in book1.
func buildServerWithCards(cards map[string]*carddav.CardInfo) *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/admin"}}}
	backend := &carddav.MockBackend{
		AddressBooks: []carddav.AddressBookInfo{{
			ID: "1", URI: "book1", PrincipalURI: "principals/admin",
			Props: map[xmlName]any{{Space: "DAV:", Local: "displayname"}: "Contacts"},
		}},
		CardsByBook: map[string]map[string]*carddav.CardInfo{"1": cards},
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

// TestAddressbookMultigetEncodedHref covers the bug where multigetReport
// (internal/carddav/plugin.go) strips the base URI from each href with a bare
// strings.TrimPrefix instead of resolving it like CalDAV's
// server.ResolveHref, so percent-encoded hrefs — exactly the form the server
// itself emits for URIs containing spaces etc. — are never decoded and the
// card comes back 404.
func TestAddressbookMultigetEncodedHref(t *testing.T) {
	const cardSpace = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:card3\r\nFN:Carol Space\r\nEMAIL:carol@example.org\r\nEND:VCARD\r\n"
	srv := buildServerWithCards(map[string]*carddav.CardInfo{
		"my card.vcf": {URI: "my card.vcf", AddressBookID: "1", CardData: []byte(cardSpace)},
	})

	// The server emits this card's href percent-encoded; a client echoes it
	// back in the multiget verbatim, so the REPORT must decode it.
	mg := `<?xml version="1.0"?><c:addressbook-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/><c:address-data/></d:prop>
	  <d:href>/addressbooks/admin/book1/my%20card.vcf</d:href>
	</c:addressbook-multiget>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", mg, "Content-Type", "application/xml")
	if w.Code != 207 || !strings.Contains(w.Body.String(), "Carol Space") {
		t.Errorf("multiget with percent-encoded href = %d, want address-data for the card:\n%s", w.Code, w.Body.String())
	}
}

// TestAddressbookMultigetAbsoluteHref covers the same bug for the
// absolute-URL href form RFC 6352 allows: CalDAV's ResolveHref strips the
// scheme/host, the CardDAV multiget's bare TrimPrefix does not.
func TestAddressbookMultigetAbsoluteHref(t *testing.T) {
	srv := buildServer()
	mg := `<?xml version="1.0"?><c:addressbook-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/><c:address-data/></d:prop>
	  <d:href>http://example.com/addressbooks/admin/book1/card2.vcf</d:href>
	</c:addressbook-multiget>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", mg, "Content-Type", "application/xml")
	if w.Code != 207 || !strings.Contains(w.Body.String(), "Bob Jones") {
		t.Errorf("multiget with absolute-URL href = %d, want address-data for card2:\n%s", w.Code, w.Body.String())
	}
}

// TestAddressbookQueryNegateTextMatch covers the bug where the text-match
// negation (internal/carddav/query.go, textMatch.matches) is applied per
// property instance instead of after the any-instance-matches quantifier.
// Sabre semantics: a negated text-match succeeds only if NO instance of the
// property matches; a card with EMAIL a@x.com plus a second EMAIL must not
// be returned by negate-condition="yes" on a@x.com.
func TestAddressbookQueryNegateTextMatch(t *testing.T) {
	const cardMulti = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:card4\r\nFN:Dave Multi\r\nEMAIL:a@x.com\r\nEMAIL:b@y.com\r\nEND:VCARD\r\n"
	srv := buildServerWithCards(map[string]*carddav.CardInfo{
		"card1.vcf": {URI: "card1.vcf", AddressBookID: "1", CardData: []byte(card1)},
		"multi.vcf": {URI: "multi.vcf", AddressBookID: "1", CardData: []byte(cardMulti)},
	})
	query := `<?xml version="1.0"?><c:addressbook-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
	  <d:prop><d:getetag/></d:prop>
	  <c:filter><c:prop-filter name="EMAIL"><c:text-match match-type="equals" negate-condition="yes">a@x.com</c:text-match></c:prop-filter></c:filter>
	</c:addressbook-query>`
	w := do(t, srv, "REPORT", "/addressbooks/admin/book1/", query, "Depth", "1", "Content-Type", "application/xml")
	if w.Code != 207 {
		t.Fatalf("query status = %d:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "card1.vcf") {
		t.Errorf("negated match missed card1 (no EMAIL equals a@x.com):\n%s", body)
	}
	if strings.Contains(body, "multi.vcf") {
		t.Errorf("negated match wrongly returned multi.vcf: one of its EMAILs equals a@x.com, so the negated filter must exclude it:\n%s", body)
	}
}
