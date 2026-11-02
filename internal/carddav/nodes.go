package carddav

import (
	"context"
	"encoding/xml"
	"io"
	"strings"
	"time"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/dav"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
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
func (r *AddressBookRoot) SetName(context.Context, string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (r *AddressBookRoot) Delete(ctx context.Context) error {
	return dav.ErrForbidden("Permission denied to delete node")
}
func (r *AddressBookRoot) LastModified() time.Time { return time.Time{} }

func (r *AddressBookRoot) Children(ctx context.Context) ([]dav.Node, error) {
	principals, err := r.principals.PrincipalsByPrefix(ctx, r.principalPrefix)
	if err != nil {
		return nil, err
	}
	return r.homes(principals), nil
}

// ChildrenScoped implements dav.ScopedLister: homes of the requester and its
// email domain only (acl.ScopedPrincipals).
func (r *AddressBookRoot) ChildrenScoped(ctx context.Context, scope dav.ChildScope) ([]dav.Node, error) {
	principals, err := acl.ScopedPrincipals(ctx, r.principals, r.principalPrefix, scope)
	if err != nil {
		return nil, err
	}
	return r.homes(principals), nil
}

func (r *AddressBookRoot) homes(principals []acl.Principal) []dav.Node {
	out := make([]dav.Node, len(principals))
	for i, p := range principals {
		home := NewAddressBookHome(r.backend, p.URI)
		home.autoProvision = r.AutoProvision
		out[i] = home
	}
	return out
}

func (r *AddressBookRoot) Child(ctx context.Context, name string) (dav.Node, error) {
	p, err := r.principals.PrincipalByPath(ctx, r.principalPrefix+"/"+name)
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

func (r *AddressBookRoot) ChildExists(ctx context.Context, name string) (bool, error) {
	_, err := r.Child(ctx, name)
	return dav.Found(err)
}

func (r *AddressBookRoot) CreateFile(context.Context, string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in this collection is not supported")
}
func (r *AddressBookRoot) CreateDirectory(context.Context, string) error {
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
func (h *AddressBookHome) SetName(context.Context, string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (h *AddressBookHome) Delete(ctx context.Context) error {
	return dav.ErrForbidden("Permission denied to delete node")
}
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

func (h *AddressBookHome) Children(ctx context.Context) ([]dav.Node, error) {
	infos, err := h.provisioned(ctx)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, info := range infos {
		out = append(out, NewAddressBook(h.backend, info))
	}
	return out, nil
}

func (h *AddressBookHome) Child(ctx context.Context, name string) (dav.Node, error) {
	infos, err := h.provisioned(ctx)
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

func (h *AddressBookHome) ChildExists(ctx context.Context, name string) (bool, error) {
	_, err := h.Child(ctx, name)
	return dav.Found(err)
}

// provisioned lists the principal's address books, recreating the default
// "addressbook" first when auto-provisioning is on and none are left
// (Calendar-Server behavior the CalDAVTester suite relies on).
func (h *AddressBookHome) provisioned(ctx context.Context) ([]AddressBookInfo, error) {
	infos, err := h.backend.AddressBooksForUser(ctx, h.principalURI)
	if err != nil || !h.autoProvision || len(infos) > 0 {
		return infos, err
	}
	if _, err := h.backend.CreateAddressBook(ctx, h.principalURI, "addressbook", map[xml.Name]any{
		{Space: davxml.NSDav, Local: "displayname"}: "addressbook",
	}); err != nil {
		return nil, err
	}
	return h.backend.AddressBooksForUser(ctx, h.principalURI)
}

func (h *AddressBookHome) CreateFile(context.Context, string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in this collection is not supported")
}

func (h *AddressBookHome) CreateDirectory(ctx context.Context, name string) error {
	// Plain MKCOL creates an address book (sabre allows this on the home).
	_, err := h.backend.CreateAddressBook(ctx, h.principalURI, name, map[xml.Name]any{})
	return err
}

// CreateExtendedCollection implements dav.ExtendedCollection: extended MKCOL
// with the {CARDDAV:}addressbook resource type.
func (h *AddressBookHome) CreateExtendedCollection(ctx context.Context, name string, resourceTypes []xml.Name, props map[xml.Name]any) error {
	_, err := h.backend.CreateAddressBook(ctx, h.principalURI, name, props)
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
func (a *AddressBook) SetName(context.Context, string) error {
	return dav.ErrMethodNotAllowed("Renaming addressbooks is not yet supported")
}
func (a *AddressBook) Delete(ctx context.Context) error {
	return a.backend.DeleteAddressBook(ctx, a.Info.ID)
}
func (a *AddressBook) LastModified() time.Time { return time.Time{} }
func (a *AddressBook) Owner() string           { return a.Info.PrincipalURI }

func (a *AddressBook) Children(ctx context.Context) ([]dav.Node, error) {
	cards, err := a.backend.Cards(ctx, a.Info.ID)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, card := range cards {
		out = append(out, NewCard(a.backend, a.Info, card))
	}
	return out, nil
}

func (a *AddressBook) Child(ctx context.Context, name string) (dav.Node, error) {
	card, err := a.backend.Card(ctx, a.Info.ID, name)
	if err != nil {
		return nil, err
	}
	if card == nil {
		return nil, dav.ErrNotFound("Card not found")
	}
	return NewCard(a.backend, a.Info, *card), nil
}

func (a *AddressBook) ChildExists(ctx context.Context, name string) (bool, error) {
	_, err := a.Child(ctx, name)
	return dav.Found(err)
}

func (a *AddressBook) CreateFile(ctx context.Context, name string, data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	return a.backend.CreateCard(ctx, a.Info.ID, name, raw)
}

func (a *AddressBook) CreateDirectory(context.Context, string) error {
	return dav.ErrMethodNotAllowed("Creating collections in an address book is not allowed")
}

func (a *AddressBook) Props(ctx context.Context, names []xml.Name) (map[xml.Name]any, error) {
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

func (a *AddressBook) PropPatch(ctx context.Context, pp *dav.PropPatch) {
	a.backend.UpdateAddressBook(ctx, a.Info.ID, pp)
}

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

func (a *AddressBook) Changes(ctx context.Context, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	return a.backend.ChangesForAddressBook(ctx, a.Info.ID, syncToken, syncLevel, limit)
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
func (c *Card) SetName(context.Context, string) error {
	// Sabre's Card inherits DAV\Node::setName (via File), which forbids
	// renaming; a MOVE-rename of a card therefore gets 403, matching sabre.
	return dav.ErrForbidden("Permission denied to rename file")
}
func (c *Card) Delete(ctx context.Context) error {
	return c.backend.DeleteCard(ctx, c.addressBook.ID, c.info.URI)
}
func (c *Card) LastModified() time.Time { return time.Time{} }

func (c *Card) Get(ctx context.Context) (io.ReadCloser, error) {
	return dav.BytesBody(c.info.CardData), nil
}

func (c *Card) Put(ctx context.Context, data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	etag, err := c.backend.UpdateCard(ctx, c.addressBook.ID, c.info.URI, raw)
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
