package acl

import (
	"encoding/xml"
	"io"
	"regexp"
	"strings"
	"time"

	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// UserPrincipalNode is a user principal that also acts as a collection hosting
// its calendar-proxy-read/-write sub-principals. Port of sabre's
// CalDAV\Principal\User. Used only when proxy delegation is enabled, so plain
// DAVACL principals stay leaf nodes.
type UserPrincipalNode struct {
	*PrincipalNode
}

func (u *UserPrincipalNode) proxyChild(name string) (*ProxyPrincipalNode, bool) {
	if name == proxyReadName || name == proxyWriteName {
		return &ProxyPrincipalNode{
			backend:  u.backend,
			uri:      u.principal.URI + "/" + name,
			readOnly: name == proxyReadName,
		}, true
	}
	return nil, false
}

func (u *UserPrincipalNode) Child(name string) (dav.Node, error) {
	if child, ok := u.proxyChild(name); ok {
		return child, nil
	}
	return nil, dav.ErrNotFound("Principal %s has no child named %s", u.principal.URI, name)
}

func (u *UserPrincipalNode) Children() ([]dav.Node, error) {
	read, _ := u.proxyChild(proxyReadName)
	write, _ := u.proxyChild(proxyWriteName)
	return []dav.Node{read, write}, nil
}

func (u *UserPrincipalNode) ChildExists(name string) bool {
	return name == proxyReadName || name == proxyWriteName
}

func (u *UserPrincipalNode) CreateFile(string, io.Reader) (string, error) {
	return "", dav.ErrForbidden("Permission denied to create file")
}

func (u *UserPrincipalNode) CreateDirectory(string) error {
	return dav.ErrForbidden("Permission denied to create directory")
}

// The two calendar-proxy group principals that live under every user
// principal (calendarserver delegation extension).
const (
	proxyReadName  = "calendar-proxy-read"
	proxyWriteName = "calendar-proxy-write"
)

// ProxyPrincipalNode is a calendar-proxy-read / -write group principal beneath a
// user principal. Its group-member-set is the set of principals granted proxy
// (delegate) access to the user's calendars — read-only for -read, read-write
// for -write. Port of sabre's CalDAV\Principal\ProxyRead / ProxyWrite.
type ProxyPrincipalNode struct {
	backend  Backend
	uri      string
	readOnly bool
}

func (p *ProxyPrincipalNode) Name() string {
	_, name := splitPath(p.uri)
	return name
}

func (p *ProxyPrincipalNode) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}

func (p *ProxyPrincipalNode) Delete() error {
	return dav.ErrForbidden("Permission denied to delete node")
}

func (p *ProxyPrincipalNode) LastModified() time.Time { return time.Time{} }

func (p *ProxyPrincipalNode) PrincipalURL() string      { return p.uri }
func (p *ProxyPrincipalNode) AlternateURISet() []string { return nil }

func (p *ProxyPrincipalNode) GroupMemberSet() ([]string, error) {
	return p.backend.GroupMemberSet(p.uri)
}

func (p *ProxyPrincipalNode) GroupMembership() ([]string, error) {
	return p.backend.GroupMembership(p.uri)
}

// ProxyResourceType is the calendarserver resourcetype element identifying this
// node as a read or write proxy principal.
func (p *ProxyPrincipalNode) ProxyResourceType() xml.Name {
	local := "calendar-proxy-write"
	if p.readOnly {
		local = "calendar-proxy-read"
	}
	return xml.Name{Space: davxml.NSCalendarServer, Local: local}
}

func (p *ProxyPrincipalNode) Props(names []xml.Name) (map[xml.Name]any, error) {
	out := map[xml.Name]any{}
	for _, name := range names {
		// Calendar Server tags proxy groups with a record-type.
		if name == (xml.Name{Space: davxml.NSCalendarServer, Local: "record-type"}) {
			if p.readOnly {
				out[name] = "read-delegate-group"
			} else {
				out[name] = "write-delegate-group"
			}
		}
	}
	return out, nil
}

// PropPatch handles group-member-set writes: this is how a client (or the
// calendar owner) configures who may act as their delegate.
func (p *ProxyPrincipalNode) PropPatch(pp *dav.PropPatch) {
	name := xml.Name{Space: davxml.NSDav, Local: "group-member-set"}
	pp.Handle([]xml.Name{name}, func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		if err := p.backend.SetGroupMemberSet(p.uri, parseHrefs(values[name])); err != nil {
			return nil, false
		}
		return nil, true
	})
}

// ACL grants the owning user full control of their own proxy settings.
func (p *ProxyPrincipalNode) ACL() []ACE {
	return []ACE{{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true}}
}

// Owner is the parent user principal.
func (p *ProxyPrincipalNode) Owner() string {
	parent, _ := splitPath(p.uri)
	return parent
}

var hrefRe = regexp.MustCompile(`(?i)<(?:[a-z0-9]+:)?href[^>]*>\s*([^<\s][^<]*?)\s*</(?:[a-z0-9]+:)?href\s*>`)

// parseHrefs extracts principal paths from a group-member-set PROPPATCH value
// (a fragment of <href> elements). Hrefs are normalized to bare principal paths
// (leading/trailing slashes trimmed), matching how principals are stored.
func parseHrefs(value any) []string {
	var raw []byte
	switch v := value.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return nil
	}
	var out []string
	for _, m := range hrefRe.FindAllSubmatch(raw, -1) {
		if href := strings.Trim(string(m[1]), "/ \t\r\n"); href != "" {
			out = append(out, href)
		}
	}
	return out
}
