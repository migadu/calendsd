package dav

import (
	"io"
	"slices"
	"time"
)

// Root is the fixed top of the tree: a read-only collection over the
// children it is built with. Nothing can be created, renamed or deleted at
// this level, whatever the ACL says.
type Root struct {
	children []Node
}

// NewRoot builds the root over a fixed set of children.
func NewRoot(children ...Node) *Root {
	return &Root{children: children}
}

func (r *Root) Name() string            { return "root" }
func (r *Root) LastModified() time.Time { return time.Time{} }

func (r *Root) SetName(string) error {
	return ErrForbidden("The root collection cannot be renamed")
}

func (r *Root) Delete() error {
	return ErrForbidden("The root collection cannot be deleted")
}

func (r *Root) Child(name string) (Node, error) {
	for _, ch := range r.children {
		if ch.Name() == name {
			return ch, nil
		}
	}
	return nil, ErrNotFound("File not found: %s in 'root'", name)
}

func (r *Root) Children() ([]Node, error) { return slices.Clone(r.children), nil }

func (r *Root) ChildExists(name string) bool {
	_, err := r.Child(name)
	return err == nil
}

func (r *Root) CreateFile(name string, _ io.Reader) (string, error) {
	return "", ErrForbidden("Cannot create %s in the root collection", name)
}

func (r *Root) CreateDirectory(name string) error {
	return ErrForbidden("Cannot create %s in the root collection", name)
}
