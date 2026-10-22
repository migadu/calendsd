package carddav

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// AddressBookRoot is the /addressbooks collection: one home per principal.
// Port of sabre's CardDAV\AddressBookRoot.
type AddressBookRoot struct {
	name            string
	principals      acl.Backend
	backend         Backend
	principalPrefix string
	// AutoProvision recreates the default "addressbook" when a principal
	// has none left (Calendar-Server behavior); off by default.
	AutoProvision bool
}

// NewAddressBookRoot builds the address book root (conventionally
// "addressbooks").
func NewAddressBookRoot(principals acl.Backend, backend Backend, name string) *AddressBookRoot {
	return &AddressBookRoot{name: name, principals: principals, backend: backend, principalPrefix: "principals"}
}

func (r *AddressBookRoot) Name() string { return r.name }
func (r *AddressBookRoot) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (r *AddressBookRoot) Delete() error           { return dav.ErrForbidden("Permission denied to delete node") }
func (r *AddressBookRoot) LastModified() time.Time { return time.Time{} }

func (r *AddressBookRoot) Children() ([]dav.Node, error) {
	principals, err := r.principals.PrincipalsByPrefix(r.principalPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]dav.Node, len(principals))
	for i, p := range principals {
		home := NewAddressBookHome(r.backend, p.URI)
		home.autoProvision = r.AutoProvision
		out[i] = home
	}
	return out, nil
}

func (r *AddressBookRoot) Child(name string) (dav.Node, error) {
	p, err := r.principals.PrincipalByPath(r.principalPrefix + "/" + name)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, dav.ErrNotFound("Principal with name %s not found", name)
	}
	home := NewAddressBookHome(r.backend, p.URI)
	home.autoProvision = r.AutoProvision
	return home, nil
}

func (r *AddressBookRoot) ChildExists(name string) bool {
	p, err := r.principals.PrincipalByPath(r.principalPrefix + "/" + name)
	return err == nil && p != nil
}

func (r *AddressBookRoot) CreateFile(string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in this collection is not supported")
}
func (r *AddressBookRoot) CreateDirectory(string) error {
	return dav.ErrMethodNotAllowed("Creating collections in this collection is not supported")
}

// AddressBookHome holds one principal's address books. Port of sabre's
// CardDAV\AddressBookHome.
type AddressBookHome struct {
	backend       Backend
	principalURI  string
	autoProvision bool
}

// NewAddressBookHome builds the home for a principal.
func NewAddressBookHome(backend Backend, principalURI string) *AddressBookHome {
	return &AddressBookHome{backend: backend, principalURI: principalURI}
}

func (h *AddressBookHome) Name() string {
	i := strings.LastIndexByte(h.principalURI, '/')
	return h.principalURI[i+1:]
}
func (h *AddressBookHome) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (h *AddressBookHome) Delete() error           { return dav.ErrForbidden("Permission denied to delete node") }
func (h *AddressBookHome) LastModified() time.Time { return time.Time{} }
func (h *AddressBookHome) Owner() string           { return h.principalURI }

// ACL mirrors sabre's AddressBookHome (ACLTrait::getACL): {DAV:}owner holds
// {DAV:}all. Being an ACLHolder also puts {DAV:}write-acl in the home's
// supported-privilege set, matching sabre's current-user-privilege-set.
func (h *AddressBookHome) ACL() []acl.ACE {
	return []acl.ACE{
		{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true},
	}
}

func (h *AddressBookHome) Children() ([]dav.Node, error) {
	infos, err := h.provisioned()
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, info := range infos {
		out = append(out, NewAddressBook(h.backend, info))
	}
	return out, nil
}

func (h *AddressBookHome) Child(name string) (dav.Node, error) {
	infos, err := h.provisioned()
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if info.URI == name {
			return NewAddressBook(h.backend, info), nil
		}
	}
	return nil, dav.ErrNotFound("Node with name '%s' could not be found", name)
}

func (h *AddressBookHome) ChildExists(name string) bool {
	_, err := h.Child(name)
	return err == nil
}

// provisioned lists the principal's address books, recreating the default
// "addressbook" first when auto-provisioning is on and none are left
// (Calendar-Server behavior the CalDAVTester suite relies on).
func (h *AddressBookHome) provisioned() ([]AddressBookInfo, error) {
	infos, err := h.backend.AddressBooksForUser(h.principalURI)
	if err != nil || !h.autoProvision || len(infos) > 0 {
		return infos, err
	}
	if _, err := h.backend.CreateAddressBook(h.principalURI, "addressbook", map[xml.Name]any{
		{Space: davxml.NSDav, Local: "displayname"}: "addressbook",
	}); err != nil {
		return nil, err
	}
	return h.backend.AddressBooksForUser(h.principalURI)
}

func (h *AddressBookHome) CreateFile(string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in this collection is not supported")
}

func (h *AddressBookHome) CreateDirectory(name string) error {
	// Plain MKCOL creates an address book (sabre allows this on the home).
	_, err := h.backend.CreateAddressBook(h.principalURI, name, map[xml.Name]any{})
	return err
}

// CreateExtendedCollection implements dav.ExtendedCollection: extended MKCOL
// with the {CARDDAV:}addressbook resource type.
func (h *AddressBookHome) CreateExtendedCollection(name string, resourceTypes []xml.Name, props map[xml.Name]any) error {
	_, err := h.backend.CreateAddressBook(h.principalURI, name, props)
	return err
}

// AddressBook is one address book collection. Port of sabre's
// CardDAV\AddressBook.
type AddressBook struct {
	backend Backend
	Info    AddressBookInfo
}

// NewAddressBook wraps address book info into a node.
func NewAddressBook(backend Backend, info AddressBookInfo) *AddressBook {
	return &AddressBook{backend: backend, Info: info}
}

func (a *AddressBook) Name() string { return a.Info.URI }
func (a *AddressBook) SetName(string) error {
	return dav.ErrMethodNotAllowed("Renaming addressbooks is not yet supported")
}
func (a *AddressBook) Delete() error           { return a.backend.DeleteAddressBook(a.Info.ID) }
func (a *AddressBook) LastModified() time.Time { return time.Time{} }
func (a *AddressBook) Owner() string           { return a.Info.PrincipalURI }

func (a *AddressBook) Children() ([]dav.Node, error) {
	cards, err := a.backend.Cards(a.Info.ID)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, card := range cards {
		out = append(out, NewCard(a.backend, a.Info, card))
	}
	return out, nil
}

func (a *AddressBook) Child(name string) (dav.Node, error) {
	card, err := a.backend.Card(a.Info.ID, name)
	if err != nil {
		return nil, err
	}
	if card == nil {
		return nil, dav.ErrNotFound("Card not found")
	}
	return NewCard(a.backend, a.Info, *card), nil
}

func (a *AddressBook) ChildExists(name string) bool {
	card, err := a.backend.Card(a.Info.ID, name)
	return err == nil && card != nil
}

func (a *AddressBook) CreateFile(name string, data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	return a.backend.CreateCard(a.Info.ID, name, raw)
}

func (a *AddressBook) CreateDirectory(string) error {
	return dav.ErrMethodNotAllowed("Creating collections in an address book is not allowed")
}

func (a *AddressBook) Props(names []xml.Name) (map[xml.Name]any, error) {
	if len(names) == 0 {
		return a.Info.Props, nil
	}
	out := map[xml.Name]any{}
	for _, n := range names {
		if v, ok := a.Info.Props[n]; ok {
			out[n] = v
		}
	}
	return out, nil
}

func (a *AddressBook) PropPatch(pp *dav.PropPatch) { a.backend.UpdateAddressBook(a.Info.ID, pp) }

// ACL grants the owner full access, like sabre's AddressBook::getACL.
func (a *AddressBook) ACL() []acl.ACE {
	// sabre's AddressBook uses ACLTrait::getACL — {DAV:}owner gets {DAV:}all
	// (which aggregates read+write, so authorization is unchanged, but the
	// reported privilege set now includes {DAV:}all like sabre).
	return []acl.ACE{
		{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true},
	}
}

// SyncToken implements dav.SyncCollection from the sabredav sync-token prop.
func (a *AddressBook) SyncToken() string {
	if v, ok := a.Info.Props[xml.Name{Space: "http://sabredav.org/ns", Local: "sync-token"}].(string); ok {
		return v
	}
	return ""
}

func (a *AddressBook) Changes(syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	return a.backend.ChangesForAddressBook(a.Info.ID, syncToken, syncLevel, limit)
}

// Card is one vCard object. Port of sabre's CardDAV\Card.
type Card struct {
	backend     Backend
	addressBook AddressBookInfo
	info        CardInfo
}

// NewCard wraps card info into a file node.
func NewCard(backend Backend, addressBook AddressBookInfo, info CardInfo) *Card {
	return &Card{backend: backend, addressBook: addressBook, info: info}
}

func (c *Card) Name() string { return c.info.URI }
func (c *Card) SetName(string) error {
	// Sabre's Card inherits DAV\Node::setName (via File), which forbids
	// renaming; a MOVE-rename of a card therefore gets 403, matching sabre.
	return dav.ErrForbidden("Permission denied to rename file")
}
func (c *Card) Delete() error           { return c.backend.DeleteCard(c.addressBook.ID, c.info.URI) }
func (c *Card) LastModified() time.Time { return time.Time{} }

func (c *Card) Get() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(c.info.CardData)), nil
}

func (c *Card) Put(data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	etag, err := c.backend.UpdateCard(c.addressBook.ID, c.info.URI, raw)
	if err != nil {
		return "", err
	}
	c.info.CardData = raw
	return etag, nil
}

func (c *Card) ContentType() string { return "text/vcard; charset=utf-8" }
func (c *Card) Size() int64         { return int64(len(c.info.CardData)) }
func (c *Card) ETag() string        { return c.info.QuotedETag() }
func (c *Card) Data() []byte        { return c.info.CardData }
func (c *Card) Owner() string       { return c.addressBook.PrincipalURI }

// ACL grants the address book's owner full access, like sabre's
// Card::getACL. Without it the card fell back to the ACL plugin's default.
func (c *Card) ACL() []acl.ACE {
	return []acl.ACE{
		{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true},
	}
}
