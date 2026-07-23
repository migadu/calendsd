// Package acl implements principals and access control: principal nodes, the
// principal backend abstraction, and the ACL plugin. Port of sabre's
// lib/DAVACL.
package acl

import (
	"encoding/xml"

	"calserver/internal/dav"
)

// Principal is one principal's identity and properties.
type Principal struct {
	// URI is the principal path, e.g. "principals/admin".
	URI string
	// Props carries principal properties ({DAV:}displayname, the sabredav
	// email-address extension, ...).
	Props map[xml.Name]any
}

// Backend provides principal storage. Port of sabre's
// PrincipalBackend\BackendInterface (the subset the MVP needs).
type Backend interface {
	// PrincipalsByPrefix lists principals directly under a prefix such as
	// "principals".
	PrincipalsByPrefix(prefix string) ([]Principal, error)
	// PrincipalByPath returns the principal at path, or nil.
	PrincipalByPath(path string) (*Principal, error)
	// GroupMemberSet lists members of a group principal.
	GroupMemberSet(principal string) ([]string, error)
	// GroupMembership lists groups a principal belongs to.
	GroupMembership(principal string) ([]string, error)
}

// MemoryBackend is an in-memory principal backend, used by tests and the
// corpus runner. Port of sabre's PrincipalBackend\Mock.
type MemoryBackend struct {
	Principals []Principal
	// Groups maps group principal path to member paths.
	Groups map[string][]string
}

func (m *MemoryBackend) PrincipalsByPrefix(prefix string) ([]Principal, error) {
	var out []Principal
	for _, p := range m.Principals {
		if parent, _ := splitPath(p.URI); parent == prefix {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *MemoryBackend) PrincipalByPath(path string) (*Principal, error) {
	for _, p := range m.Principals {
		if p.URI == path {
			return &p, nil
		}
	}
	return nil, nil
}

func (m *MemoryBackend) GroupMemberSet(principal string) ([]string, error) {
	return m.Groups[principal], nil
}

func (m *MemoryBackend) GroupMembership(principal string) ([]string, error) {
	var out []string
	for group, members := range m.Groups {
		for _, member := range members {
			if member == principal {
				out = append(out, group)
			}
		}
	}
	return out, nil
}

func splitPath(path string) (parent, base string) {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i], path[i+1:]
		}
	}
	return "", path
}

var _ dav.Node = (*PrincipalNode)(nil)
