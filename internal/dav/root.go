package dav

import (
	"context"
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

func (r *Root) SetName(context.Context, string) error {
	return ErrForbidden("The root collection cannot be renamed")
}

func (r *Root) Delete(ctx context.Context) error {
	return ErrForbidden("The root collection cannot be deleted")
}

func (r *Root) Child(ctx context.Context, name string) (Node, error) {
	for _, ch := range r.children {
		if ch.Name() == name {
			return ch, nil
		}
	}
	return nil, ErrNotFound("File not found: %s in 'root'", name)
}

func (r *Root) Children(ctx context.Context) ([]Node, error) { return slices.Clone(r.children), nil }

func (r *Root) ChildExists(ctx context.Context, name string) (bool, error) {
	_, err := r.Child(ctx, name)
	return Found(err)
}

func (r *Root) CreateFile(ctx context.Context, name string, _ io.Reader) (string, error) {
	return "", ErrForbidden("Cannot create %s in the root collection", name)
}

func (r *Root) CreateDirectory(ctx context.Context, name string) error {
	return ErrForbidden("Cannot create %s in the root collection", name)
}
