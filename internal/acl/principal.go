package acl

import (
	"encoding/xml"
	"io"
	"time"

	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// PrincipalHolder marks nodes representing a principal (sabre's IPrincipal).
type PrincipalHolder interface {
	PrincipalURL() string
	AlternateURISet() []string
	GroupMemberSet() ([]string, error)
	GroupMembership() ([]string, error)
}

// ACE is one access control entry.
type ACE struct {
	// Privilege is a clark-notation privilege, e.g. "{DAV:}all".
	Privilege string
	// Principal is a principal path, or a pseudo-principal in clark
	// notation ("{DAV:}owner", "{DAV:}authenticated").
	Principal string
	Protected bool
}

// ACLHolder is a node carrying its own ACL (sabre's IACL subset).
type ACLHolder interface {
	ACL() []ACE
}

// PrincipalNode is a principal in the DAV tree. Port of sabre's
// DAVACL\Principal.
type PrincipalNode struct {
	backend   Backend
	principal Principal
}

// NewPrincipalNode wraps a principal into a node.
func NewPrincipalNode(backend Backend, principal Principal) *PrincipalNode {
	return &PrincipalNode{backend: backend, principal: principal}
}

func (p *PrincipalNode) Name() string {
	_, name := splitPath(p.principal.URI)
	return name
}

func (p *PrincipalNode) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}

func (p *PrincipalNode) Delete() error {
	return dav.ErrForbidden("Permission denied to delete node")
}

func (p *PrincipalNode) LastModified() time.Time { return time.Time{} }

func (p *PrincipalNode) PrincipalURL() string { return p.principal.URI }

func (p *PrincipalNode) AlternateURISet() []string {
	var out []string
	if v, ok := p.principal.Props[xml.Name{Space: davxml.NSDav, Local: "alternate-URI-set"}].([]string); ok {
		out = append(out, v...)
	}
	if email, ok := p.principal.Props[xml.Name{Space: davxml.NSSabreDAV, Local: "email-address"}].(string); ok {
		out = append(out, "mailto:"+email)
	}
	return out
}

func (p *PrincipalNode) GroupMemberSet() ([]string, error) {
	return p.backend.GroupMemberSet(p.principal.URI)
}

func (p *PrincipalNode) GroupMembership() ([]string, error) {
	return p.backend.GroupMembership(p.principal.URI)
}

// Props implements dav.PropNode with the stored principal properties.
func (p *PrincipalNode) Props(names []xml.Name) (map[xml.Name]any, error) {
	if len(names) == 0 {
		return p.principal.Props, nil
	}
	out := map[xml.Name]any{}
	for _, n := range names {
		if v, ok := p.principal.Props[n]; ok {
			out[n] = v
		}
	}
	return out, nil
}

func (p *PrincipalNode) PropPatch(pp *dav.PropPatch) {}

// ACL implements ACLHolder: the principal itself has full access, mirroring
// sabre's ACLTrait default.
func (p *PrincipalNode) ACL() []ACE {
	return []ACE{{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true}}
}

// Owner returns the owner principal path (the principal itself).
func (p *PrincipalNode) Owner() string { return p.principal.URI }

// PrincipalCollection exposes all principals under a prefix. Port of sabre's
// PrincipalCollection/AbstractPrincipalCollection.
type PrincipalCollection struct {
	backend Backend
	// prefix is the principal path prefix, conventionally "principals".
	prefix string
}

// NewPrincipalCollection creates the collection for a prefix.
func NewPrincipalCollection(backend Backend, prefix string) *PrincipalCollection {
	return &PrincipalCollection{backend: backend, prefix: prefix}
}

func (c *PrincipalCollection) Name() string {
	_, name := splitPath(c.prefix)
	return name
}

func (c *PrincipalCollection) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}

func (c *PrincipalCollection) Delete() error {
	return dav.ErrForbidden("Permission denied to delete node")
}

func (c *PrincipalCollection) LastModified() time.Time { return time.Time{} }

func (c *PrincipalCollection) Child(name string) (dav.Node, error) {
	p, err := c.backend.PrincipalByPath(c.prefix + "/" + name)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, dav.ErrNotFound("Principal with name %s not found", name)
	}
	return NewPrincipalNode(c.backend, *p), nil
}

func (c *PrincipalCollection) Children() ([]dav.Node, error) {
	principals, err := c.backend.PrincipalsByPrefix(c.prefix)
	if err != nil {
		return nil, err
	}
	out := make([]dav.Node, len(principals))
	for i, p := range principals {
		out[i] = NewPrincipalNode(c.backend, p)
	}
	return out, nil
}

func (c *PrincipalCollection) ChildExists(name string) bool {
	p, err := c.backend.PrincipalByPath(c.prefix + "/" + name)
	return err == nil && p != nil
}

func (c *PrincipalCollection) CreateFile(string, io.Reader) (string, error) {
	return "", dav.ErrForbidden("Permission denied to create file (filename )")
}

func (c *PrincipalCollection) CreateDirectory(string) error {
	return dav.ErrForbidden("Permission denied to create directory")
}

// SearchPrincipals implements PrincipalSearcher via the backend.
func (c *PrincipalCollection) SearchPrincipals(search map[xml.Name]string, test string) ([]string, error) {
	return c.backend.SearchPrincipals(c.prefix, search, test)
}
