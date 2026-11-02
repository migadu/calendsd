package dav

import (
	"context"
	"io"
	"strings"

	"github.com/migadu/calendsd/internal/pathutil"
)

// DepthInfinity is the parsed value of "Depth: infinity".
const DepthInfinity = -1

// Tree resolves URL paths to nodes and implements the recursive copy/move
// operations. Port of sabre's DAV\Tree (without the node cache: in-memory
// backends make lookups cheap, and the cache invalidation subtleties aren't
// worth porting until a profile says otherwise).
type Tree struct {
	root Collection
}

// NewTree creates a tree over the given root collection.
func NewTree(root Collection) *Tree {
	return &Tree{root: root}
}

// NodeForPath resolves a server-relative path ("" is the root).
func (t *Tree) NodeForPath(ctx context.Context, path string) (Node, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		return t.root, nil
	}
	var node Node = t.root
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			continue
		}
		coll, isColl := node.(Collection)
		if !isColl {
			return nil, ErrNotFound("Could not find node at path: %s", path)
		}
		child, err := coll.Child(ctx, part)
		if err != nil {
			return nil, err
		}
		node = child
	}
	return node, nil
}

// NodeExists reports whether a node exists at path. A missing parent is a
// false; a failed lookup is an error.
func (t *Tree) NodeExists(ctx context.Context, path string) (bool, error) {
	if strings.Trim(path, "/") == "" {
		return true, nil
	}
	parent, base := pathutil.Split(strings.Trim(path, "/"))
	parentNode, err := t.NodeForPath(ctx, parent)
	if err != nil {
		return Found(err)
	}
	coll, isColl := parentNode.(Collection)
	if !isColl {
		return false, nil
	}
	return coll.ChildExists(ctx, base)
}

// Delete removes the node at path.
func (t *Tree) Delete(ctx context.Context, path string) error {
	node, err := t.NodeForPath(ctx, path)
	if err != nil {
		return err
	}
	return node.Delete(ctx)
}

// pathContainsOrEquals reports whether descendant is ancestor itself or lives
// beneath it. The root ("") contains everything.
func pathContainsOrEquals(ancestor, descendant string) bool {
	ancestor = strings.Trim(ancestor, "/")
	descendant = strings.Trim(descendant, "/")
	if ancestor == "" {
		return true
	}
	return descendant == ancestor || strings.HasPrefix(descendant, ancestor+"/")
}

// Copy copies the node at sourcePath to destinationPath. With depth 0 a
// collection is copied without children (RFC 4918 §9.8.3).
func (t *Tree) Copy(ctx context.Context, sourcePath, destinationPath string, depth int) error {
	// Copying a collection into itself or a descendant is forbidden (RFC 4918
	// §9.8.5); without this guard copyNode recurses into the collection it is
	// still writing to and allocates until the process is OOM-killed.
	if pathContainsOrEquals(sourcePath, destinationPath) {
		return ErrForbidden("Cannot copy a resource into itself or a descendant")
	}
	source, err := t.NodeForPath(ctx, sourcePath)
	if err != nil {
		return err
	}
	destDir, destName := pathutil.Split(strings.Trim(destinationPath, "/"))
	destParent, err := t.NodeForPath(ctx, destDir)
	if err != nil {
		return err
	}
	coll, isColl := destParent.(Collection)
	if !isColl {
		return ErrConflict("Unable to copy into a non-collection")
	}
	return t.copyNode(ctx, source, coll, destName, depth)
}

// Move moves the node at sourcePath to destinationPath. A move within one
// parent is a rename; otherwise it's copy + delete, like sabre.
func (t *Tree) Move(ctx context.Context, sourcePath, destinationPath string) error {
	if pathContainsOrEquals(sourcePath, destinationPath) {
		return ErrForbidden("Cannot move a resource into itself or a descendant")
	}
	sourceDir, _ := pathutil.Split(strings.Trim(sourcePath, "/"))
	destDir, destName := pathutil.Split(strings.Trim(destinationPath, "/"))

	if sourceDir == destDir {
		source, err := t.NodeForPath(ctx, sourcePath)
		if err != nil {
			return err
		}
		return source.SetName(ctx, destName)
	}
	if err := t.Copy(ctx, sourcePath, destinationPath, DepthInfinity); err != nil {
		return err
	}
	source, err := t.NodeForPath(ctx, sourcePath)
	if err != nil {
		return err
	}
	return source.Delete(ctx)
}

// copyNode clones source into destinationParent under destinationName,
// mirroring sabre's Tree::copyNode.
func (t *Tree) copyNode(ctx context.Context, source Node, destParent Collection, destName string, depth int) error {
	if destName == "" {
		destName = source.Name()
	}
	switch src := source.(type) {
	case File:
		data, err := src.Get(ctx)
		if err != nil {
			return err
		}
		defer data.Close()
		if _, err := destParent.CreateFile(ctx, destName, data); err != nil {
			return err
		}
	case Collection:
		if err := destParent.CreateDirectory(ctx, destName); err != nil {
			return err
		}
		if depth == 0 {
			break
		}
		destNode, err := destParent.Child(ctx, destName)
		if err != nil {
			return err
		}
		destColl, isColl := destNode.(Collection)
		if !isColl {
			return ErrConflict("Copy destination is not a collection")
		}
		children, err := src.Children(ctx)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := t.copyNode(ctx, child, destColl, "", depth); err != nil {
				return err
			}
		}
	default:
		// A node that is neither File nor Collection copies as an empty
		// file, like sabre's fallback.
		if _, err := destParent.CreateFile(ctx, destName, emptyReader{}); err != nil {
			return err
		}
	}

	if props, isProps := source.(PropNode); isProps {
		if destNode, err := destParent.Child(ctx, destName); err == nil {
			if destProps, ok := destNode.(PropNode); ok {
				copyProperties(ctx, props, destProps)
			}
		}
	}
	return nil
}

// copyProperties transfers all custom properties, matching sabre's behavior
// of replaying them through PropPatch on the destination.
func copyProperties(ctx context.Context, src, dst PropNode) {
	values, err := src.Props(ctx, nil)
	if err != nil || len(values) == 0 {
		return
	}
	pp := NewPropPatch(values)
	dst.PropPatch(ctx, pp)
	pp.Commit()
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
