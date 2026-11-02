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

// TestAutoProvisionAddressBook: with AutoProvision on, a principal whose
// address books were all deleted gets the default "addressbook" recreated on
// the next home access (Calendar-Server behavior; off by default).
func TestAutoProvisionAddressBook(t *testing.T) {
	principals := &acl.MemoryBackend{Principals: []acl.Principal{{URI: "principals/admin"}}}
	backend := &carddav.MockBackend{} // no address books at all
	abRoot := carddav.NewAddressBookRoot(principals, backend, "addressbooks")
	abRoot.AutoProvision = true
	root := mock.NewCollection("root")
	root.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root.Attach(abRoot)
	srv := dav.NewServer("/", root)
	srv.AddPlugin(auth.NewLoggedInPlugin("principals/admin"))
	srv.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv.AddPlugin(&carddav.Plugin{})

	// PUT straight into the (not-yet-existing) default book succeeds.
	req := httptest.NewRequest("PUT", "/addressbooks/admin/addressbook/a.vcf",
		strings.NewReader(card1))
	req.Header.Set("Content-Type", "text/vcard")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT into auto-provisioned book = %d: %s", w.Code, w.Body.String())
	}
	if len(backend.AddressBooks) != 1 || backend.AddressBooks[0].URI != "addressbook" {
		t.Fatalf("default book not provisioned: %+v", backend.AddressBooks)
	}

	// Without the flag, the same request 404s (sabre behavior).
	backend2 := &carddav.MockBackend{}
	abRoot2 := carddav.NewAddressBookRoot(principals, backend2, "addressbooks")
	root2 := mock.NewCollection("root")
	root2.Attach(acl.NewPrincipalCollection(principals, "principals"))
	root2.Attach(abRoot2)
	srv2 := dav.NewServer("/", root2)
	srv2.AddPlugin(auth.NewLoggedInPlugin("principals/admin"))
	srv2.AddPlugin(&acl.Plugin{AdminPrincipals: []string{"principals/admin"}})
	srv2.AddPlugin(&carddav.Plugin{})
	req = httptest.NewRequest("PUT", "/addressbooks/admin/addressbook/a.vcf",
		strings.NewReader(card1))
	req.Header.Set("Content-Type", "text/vcard")
	w = httptest.NewRecorder()
	srv2.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("PUT without auto-provision = %d, want 404", w.Code)
	}
}
