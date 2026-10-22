// Package carddav implements the CardDAV layer: address book nodes, the
// CardDAV plugin (properties + reports), the addressbook-query engine, and
// the storage backend abstraction. Port of sabre's lib/CardDAV.
package carddav

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"sort"

	"calendsd/internal/dav"
)

// AddressBookInfo describes one address book of a principal.
type AddressBookInfo struct {
	ID           string
	URI          string
	PrincipalURI string
	// Props holds clark-named address book properties (displayname,
	// addressbook-description, sync-token, ...).
	Props map[xml.Name]any
}

// CardInfo is one vCard object.
type CardInfo struct {
	URI           string
	AddressBookID string
	CardData      []byte
	ETag          string
	Size          int64
}

// QuotedETag returns the card's ETag surrounded by quotes. The Postgres
// backend always supplies one; for a card without (the in-memory backends,
// test fixtures) it is the md5 of the data, the same value storage derives.
func (c *CardInfo) QuotedETag() string {
	if c.ETag != "" {
		return c.ETag
	}
	sum := md5.Sum(c.CardData)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// Backend provides address book storage. Port of the sabre
// CardDAV\Backend\BackendInterface subset the MVP needs.
type Backend interface {
	AddressBooksForUser(ctx context.Context, principalURI string) ([]AddressBookInfo, error)
	CreateAddressBook(ctx context.Context, principalURI, uri string, props map[xml.Name]any) (string, error)
	DeleteAddressBook(ctx context.Context, id string) error
	UpdateAddressBook(ctx context.Context, id string, pp *dav.PropPatch)

	Cards(ctx context.Context, addressBookID string) ([]CardInfo, error)
	Card(ctx context.Context, addressBookID, uri string) (*CardInfo, error)
	CreateCard(ctx context.Context, addressBookID, uri string, data []byte) (string, error)
	UpdateCard(ctx context.Context, addressBookID, uri string, data []byte) (string, error)
	DeleteCard(ctx context.Context, addressBookID, uri string) error

	// ChangesForAddressBook returns changes since syncToken ("" = initial
	// sync); nil means sync is unsupported or the token is unknown.
	ChangesForAddressBook(ctx context.Context, addressBookID, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error)
}

// MockBackend is the in-memory backend used by tests and the corpus runner.
// Port of sabre's CardDAV\Backend\Mock.
type MockBackend struct {
	AddressBooks []AddressBookInfo
	// CardsByBook maps address book ID → card URI → card.
	CardsByBook map[string]map[string]*CardInfo
	nextID      int
}

func (m *MockBackend) AddressBooksForUser(ctx context.Context, principalURI string) ([]AddressBookInfo, error) {
	var out []AddressBookInfo
	for _, b := range m.AddressBooks {
		if b.PrincipalURI == principalURI {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *MockBackend) CreateAddressBook(ctx context.Context, principalURI, uri string, props map[xml.Name]any) (string, error) {
	m.nextID++
	id := "mock-ab-" + hex.EncodeToString([]byte{byte(m.nextID)})
	info := AddressBookInfo{ID: id, URI: uri, PrincipalURI: principalURI, Props: map[xml.Name]any{}}
	for k, v := range props {
		info.Props[k] = v
	}
	m.AddressBooks = append(m.AddressBooks, info)
	return id, nil
}

func (m *MockBackend) DeleteAddressBook(ctx context.Context, id string) error {
	for i, b := range m.AddressBooks {
		if b.ID == id {
			m.AddressBooks = append(m.AddressBooks[:i], m.AddressBooks[i+1:]...)
			break
		}
	}
	delete(m.CardsByBook, id)
	return nil
}

func (m *MockBackend) UpdateAddressBook(ctx context.Context, id string, pp *dav.PropPatch) {
	pp.HandleRemaining(func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		for i := range m.AddressBooks {
			if m.AddressBooks[i].ID != id {
				continue
			}
			for n, v := range values {
				if v == nil {
					delete(m.AddressBooks[i].Props, n)
				} else {
					m.AddressBooks[i].Props[n] = v
				}
			}
		}
		return nil, true
	})
}

func (m *MockBackend) Cards(ctx context.Context, addressBookID string) ([]CardInfo, error) {
	var uris []string
	for uri := range m.CardsByBook[addressBookID] {
		uris = append(uris, uri)
	}
	sort.Strings(uris) // deterministic order (map iteration is random)
	out := make([]CardInfo, 0, len(uris))
	for _, uri := range uris {
		out = append(out, *m.CardsByBook[addressBookID][uri])
	}
	return out, nil
}

func (m *MockBackend) Card(ctx context.Context, addressBookID, uri string) (*CardInfo, error) {
	c, exists := m.CardsByBook[addressBookID][uri]
	if !exists {
		return nil, nil
	}
	copied := *c
	return &copied, nil
}

func (m *MockBackend) CreateCard(ctx context.Context, addressBookID, uri string, data []byte) (string, error) {
	if m.CardsByBook == nil {
		m.CardsByBook = map[string]map[string]*CardInfo{}
	}
	if m.CardsByBook[addressBookID] == nil {
		m.CardsByBook[addressBookID] = map[string]*CardInfo{}
	}
	c := &CardInfo{URI: uri, AddressBookID: addressBookID, CardData: data}
	m.CardsByBook[addressBookID][uri] = c
	return c.QuotedETag(), nil
}

func (m *MockBackend) UpdateCard(ctx context.Context, addressBookID, uri string, data []byte) (string, error) {
	return m.CreateCard(ctx, addressBookID, uri, data)
}

func (m *MockBackend) DeleteCard(ctx context.Context, addressBookID, uri string) error {
	delete(m.CardsByBook[addressBookID], uri)
	return nil
}

func (m *MockBackend) ChangesForAddressBook(context.Context, string, string, int, int) (*dav.ChangeSet, error) {
	return nil, nil
}
