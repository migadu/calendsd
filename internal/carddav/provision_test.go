package carddav

import (
	"context"
	"encoding/xml"
	"errors"
	"testing"
)

// losingBackend plays the loser of the first-request race: another request
// creates the default address book first, so this insert fails on the
// unique URI.
type losingBackend struct{ *MockBackend }

func (b losingBackend) CreateAddressBook(ctx context.Context, principalURI, uri string, props map[xml.Name]any) (string, error) {
	if _, err := b.MockBackend.CreateAddressBook(ctx, principalURI, uri, props); err != nil {
		return "", err
	}
	return "", errors.New("duplicate key value violates unique constraint")
}

func TestProvisionLosingRaceUsesWinnersAddressBook(t *testing.T) {
	home := NewAddressBookHome(losingBackend{&MockBackend{}}, "principals/u")
	home.autoProvision = true
	exists, err := home.ChildExists(t.Context(), "addressbook")
	if err != nil || !exists {
		t.Fatalf("ChildExists(addressbook) = %v, %v; want true, nil", exists, err)
	}
}
