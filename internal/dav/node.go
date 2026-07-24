// Package dav implements the WebDAV core: server, tree, node interfaces,
// property system. It is a Go port of sabre/dav's lib/DAV.
package dav

import (
	"encoding/xml"
	"io"
	"time"
)

// Node is the base interface for every entry in the DAV tree.
// Port of sabre's INode.
type Node interface {
	// Name returns the last path segment of the node.
	Name() string
	// SetName renames the node.
	SetName(name string) error
	// Delete removes the node (recursively for collections).
	Delete() error
	// LastModified returns the node's last modification time.
	// Nodes without a known mtime return the zero time.
	LastModified() time.Time
}

// Collection is a node containing child nodes. Port of sabre's ICollection.
type Collection interface {
	Node
	// Child returns the child node with the given name, or ErrNotFound.
	Child(name string) (Node, error)
	// Children lists all child nodes.
	Children() ([]Node, error)
	// ChildExists reports whether a child with the given name exists.
	ChildExists(name string) bool
	// CreateFile creates a new file node. The returned etag, if non-empty,
	// must be surrounded by double-quotes.
	CreateFile(name string, data io.Reader) (etag string, err error)
	// CreateDirectory creates a new child collection.
	CreateDirectory(name string) error
}

// File is a leaf node carrying a payload. Port of sabre's IFile.
type File interface {
	Node
	// Get returns the file payload.
	Get() (io.ReadCloser, error)
	// Put replaces the file payload. The returned etag, if non-empty,
	// must be surrounded by double-quotes.
	Put(data io.Reader) (etag string, err error)
	// ContentType returns the MIME type, or "" if unknown.
	ContentType() string
	// Size returns the payload size in bytes.
	Size() int64
	// ETag returns the current entity tag surrounded by double-quotes,
	// or "" if unknown.
	ETag() string
}

// ExtendedCollection can create children with non-standard resource types
// (extended MKCOL / MKCALENDAR bodies). Port of sabre's IExtendedCollection.
type ExtendedCollection interface {
	Collection
	// CreateExtendedCollection creates a child with the given resource
	// types and initial properties.
	CreateExtendedCollection(name string, resourceTypes []xml.Name, props map[xml.Name]any) error
}

// PropNode is a node with custom (dead or protected) properties.
// Port of sabre's IProperties.
type PropNode interface {
	Node
	// Props returns values for the requested properties; absent entries
	// mean the node doesn't have that property. An empty request slice
	// asks for all properties (allprop).
	Props(names []xml.Name) (map[xml.Name]any, error)
	// PropPatch is invoked during PROPPATCH; the node registers handlers
	// for the mutations it supports.
	PropPatch(pp *PropPatch)
}
