package dav

import (
	"io"
	"strings"
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

// splitPath splits "a/b/c" into parent "a/b" and base "c".
func splitPath(path string) (parent, base string) {
	path = strings.Trim(path, "/")
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "", path
	}
	return path[:i], path[i+1:]
}

// NodeForPath resolves a server-relative path ("" is the root).
func (t *Tree) NodeForPath(path string) (Node, error) {
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
		child, err := coll.Child(part)
		if err != nil {
			return nil, err
		}
		node = child
	}
	return node, nil
}

// NodeExists reports whether a node exists at path.
func (t *Tree) NodeExists(path string) bool {
	if strings.Trim(path, "/") == "" {
		return true
	}
	parent, base := splitPath(path)
	parentNode, err := t.NodeForPath(parent)
	if err != nil {
		return false
	}
	coll, isColl := parentNode.(Collection)
	return isColl && coll.ChildExists(base)
}

// Delete removes the node at path.
func (t *Tree) Delete(path string) error {
	node, err := t.NodeForPath(path)
	if err != nil {
		return err
	}
	return node.Delete()
}

// Copy copies the node at sourcePath to destinationPath. With depth 0 a
// collection is copied without children (RFC 4918 §9.8.3).
func (t *Tree) Copy(sourcePath, destinationPath string, depth int) error {
	source, err := t.NodeForPath(sourcePath)
	if err != nil {
		return err
	}
	destDir, destName := splitPath(destinationPath)
	destParent, err := t.NodeForPath(destDir)
	if err != nil {
		return err
	}
	coll, isColl := destParent.(Collection)
	if !isColl {
		return ErrConflict("Unable to copy into a non-collection")
	}
	return t.copyNode(source, coll, destName, depth)
}

// Move moves the node at sourcePath to destinationPath. A move within one
// parent is a rename; otherwise it's copy + delete, like sabre.
func (t *Tree) Move(sourcePath, destinationPath string) error {
	sourceDir, _ := splitPath(sourcePath)
	destDir, destName := splitPath(destinationPath)

	if sourceDir == destDir {
		source, err := t.NodeForPath(sourcePath)
		if err != nil {
			return err
		}
		return source.SetName(destName)
	}
	if err := t.Copy(sourcePath, destinationPath, DepthInfinity); err != nil {
		return err
	}
	source, err := t.NodeForPath(sourcePath)
	if err != nil {
		return err
	}
	return source.Delete()
}

// copyNode clones source into destinationParent under destinationName,
// mirroring sabre's Tree::copyNode.
func (t *Tree) copyNode(source Node, destParent Collection, destName string, depth int) error {
	if destName == "" {
		destName = source.Name()
	}
	switch src := source.(type) {
	case File:
		data, err := src.Get()
		if err != nil {
			return err
		}
		defer data.Close()
		if _, err := destParent.CreateFile(destName, data); err != nil {
			return err
		}
	case Collection:
		if err := destParent.CreateDirectory(destName); err != nil {
			return err
		}
		if depth == 0 {
			break
		}
		destNode, err := destParent.Child(destName)
		if err != nil {
			return err
		}
		destColl, isColl := destNode.(Collection)
		if !isColl {
			return ErrConflict("Copy destination is not a collection")
		}
		children, err := src.Children()
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := t.copyNode(child, destColl, "", depth); err != nil {
				return err
			}
		}
	default:
		// A node that is neither File nor Collection copies as an empty
		// file, like sabre's fallback.
		if _, err := destParent.CreateFile(destName, emptyReader{}); err != nil {
			return err
		}
	}

	if props, isProps := source.(PropNode); isProps {
		if destNode, err := destParent.Child(destName); err == nil {
			if destProps, ok := destNode.(PropNode); ok {
				copyProperties(props, destProps)
			}
		}
	}
	return nil
}

// copyProperties transfers all custom properties, matching sabre's behavior
// of replaying them through PropPatch on the destination.
func copyProperties(src, dst PropNode) {
	values, err := src.Props(nil)
	if err != nil || len(values) == 0 {
		return
	}
	pp := NewPropPatch(values)
	dst.PropPatch(pp)
	pp.Commit()
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
