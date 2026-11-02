package carddav_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/auth"
	"github.com/migadu/calendsd/internal/carddav"
	"github.com/migadu/calendsd/internal/dav"
	"github.com/migadu/calendsd/internal/dav/mock"
)

// twoUserServer builds a CardDAV server where user1 and user2 each own an
// address book; user1's holds one card. Authenticated as loggedIn. The
// ?export GET handler carries no privilege check of its own, so this pins that
// the ACL plugin's BeforeMethod read gate protects it.
func twoUserServer(loggedIn string) *dav.Server {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{
		{URI: "principals/user1"}, {URI: "principals/user2"},
	}}
	backend := &carddav.MockBackend{
		AddressBooks: []carddav.AddressBookInfo{
			{ID: "1", URI: "book1", PrincipalURI: "principals/user1",
				Props: map[xmlName]any{{Space: "DAV:", Local: "displayname"}: "User1 Contacts"}},
			{ID: "2", URI: "book1", PrincipalURI: "principals/user2",
				Props: map[xmlName]any{{Space: "DAV:", Local: "displayname"}: "User2 Contacts"}},
		},
		CardsByBook: map[string]map[string]*carddav.CardInfo{
			"1": {"secret.vcf": {URI: "secret.vcf", AddressBookID: "1", CardData: []byte(
				"BEGIN:VCARD\r\nVERSION:3.0\r\nUID:secret\r\nFN:Secret Contact\r\nEND:VCARD\r\n")}},
		},
	}
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(carddav.NewAddressBookRoot(principals, backend, "addressbooks"))
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin(loggedIn))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&carddav.Plugin{})
	return srv
}

// TestCrossUserAddressbookExportDenied: a stranger GET ?export of another
// user's address book is refused and leaks no card data.
func TestCrossUserAddressbookExportDenied(t *testing.T) {
	srv := twoUserServer("principals/user2")

	get := httptest.NewRequest("GET", "/addressbooks/user1/book1/?export", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, get)

	if w.Code != 403 {
		t.Fatalf("stranger addressbook ?export = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Secret Contact") {
		t.Fatalf("addressbook ?export leaked card data to stranger:\n%s", w.Body.String())
	}
}

// TestOwnerAddressbookExportAllowed is the control: the gate must not break the
// owner's own export.
func TestOwnerAddressbookExportAllowed(t *testing.T) {
	srv := twoUserServer("principals/user1")

	get := httptest.NewRequest("GET", "/addressbooks/user1/book1/?export", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, get)

	if w.Code != 200 {
		t.Fatalf("owner addressbook ?export = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Secret Contact") {
		t.Fatalf("owner addressbook ?export missing own card:\n%s", w.Body.String())
	}
}

// TestStrangerCannotReadOrWriteCard: a card carries its owner's ACL, not the
// ACL plugin's default, so another user can neither read nor overwrite it.
func TestStrangerCannotReadOrWriteCard(t *testing.T) {
	srv := twoUserServer("principals/user2")

	get := httptest.NewRecorder()
	srv.ServeHTTP(get, httptest.NewRequest("GET", "/addressbooks/user1/book1/secret.vcf", nil))
	if get.Code != 403 || strings.Contains(get.Body.String(), "Secret Contact") {
		t.Fatalf("stranger GET card = %d, want 403 without card data:\n%s", get.Code, get.Body.String())
	}

	put := httptest.NewRecorder()
	srv.ServeHTTP(put, httptest.NewRequest("PUT", "/addressbooks/user1/book1/secret.vcf",
		strings.NewReader("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:secret\r\nFN:Overwritten\r\nEND:VCARD\r\n")))
	if put.Code != 403 {
		t.Fatalf("stranger PUT card = %d, want 403:\n%s", put.Code, put.Body.String())
	}

	owner := httptest.NewRecorder()
	twoUserServer("principals/user1").ServeHTTP(owner,
		httptest.NewRequest("GET", "/addressbooks/user1/book1/secret.vcf", nil))
	if owner.Code != 200 || !strings.Contains(owner.Body.String(), "Secret Contact") {
		t.Fatalf("owner GET card = %d, want 200 with the card:\n%s", owner.Code, owner.Body.String())
	}
}
