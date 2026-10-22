// Package mock provides in-memory DAV nodes, a port of sabre's test mocks
// (tests/Sabre/DAV/Mock). The corpus runner seeds these from recorded
// fixtures; unit tests use them directly.
package mock

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"io"
	"time"

	"calserver/internal/dav"
)

// Collection is an in-memory collection. Port of Sabre\DAV\Mock\Collection.
type Collection struct {
	name     string
	children []dav.Node
	parent   *Collection
}

// NewCollection builds a collection; children may be *Collection or *File.
func NewCollection(name string, children ...dav.Node) *Collection {
	c := &Collection{name: name}
	for _, child := range children {
		c.Attach(child)
	}
	return c
}

// Attach adds a node, adopting it into this collection.
func (c *Collection) Attach(n dav.Node) {
	switch v := n.(type) {
	case *Collection:
		v.parent = c
	case *PropCollection:
		v.Collection.parent = c
	case *File:
		v.parent = c
	case *StreamingFile:
		v.File.parent = c
	}
	c.children = append(c.children, n)
}

func (c *Collection) Name() string { return c.name }

func (c *Collection) SetName(name string) error {
	c.name = name
	return nil
}

func (c *Collection) LastModified() time.Time { return time.Time{} }

func (c *Collection) Child(name string) (dav.Node, error) {
	for _, ch := range c.children {
		if ch.Name() == name {
			return ch, nil
		}
	}
	return nil, dav.ErrNotFound("File not found: %s in '%s'", name, c.name)
}

func (c *Collection) Children() ([]dav.Node, error) { return c.children, nil }

func (c *Collection) ChildExists(name string) bool {
	_, err := c.Child(name)
	return err == nil
}

func (c *Collection) CreateFile(name string, data io.Reader) (string, error) {
	contents, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	f := &File{name: name, contents: contents, parent: c}
	c.children = append(c.children, f)
	return f.ETag(), nil
}

func (c *Collection) CreateDirectory(name string) error {
	c.children = append(c.children, &Collection{name: name, parent: c})
	return nil
}

func (c *Collection) DeleteChild(name string) {
	for i, ch := range c.children {
		if ch.Name() == name {
			c.children = append(c.children[:i], c.children[i+1:]...)
			return
		}
	}
}

func (c *Collection) Delete() error {
	if c.parent != nil {
		c.parent.DeleteChild(c.name)
	}
	return nil
}

// File is an in-memory file. Port of Sabre\DAV\Mock\File.
type File struct {
	name         string
	contents     []byte
	parent       *Collection
	lastModified time.Time
}

// NewFile builds a file with the given contents.
func NewFile(name, contents string) *File {
	return &File{name: name, contents: []byte(contents)}
}

// SetLastModified overrides the (zero by default) modification time.
func (f *File) SetLastModified(t time.Time) { f.lastModified = t }

func (f *File) Name() string { return f.name }

func (f *File) SetName(name string) error {
	f.name = name
	return nil
}

func (f *File) LastModified() time.Time { return f.lastModified }

func (f *File) Get() (io.ReadCloser, error) {
	return dav.BytesBody(f.contents), nil
}

func (f *File) Put(data io.Reader) (string, error) {
	contents, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	f.contents = contents
	return f.ETag(), nil
}

func (f *File) ContentType() string { return "" }

func (f *File) Size() int64 { return int64(len(f.contents)) }

func (f *File) ETag() string {
	sum := md5.Sum(f.contents)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (f *File) Delete() error {
	if f.parent != nil {
		f.parent.DeleteChild(f.name)
	}
	return nil
}

// MemoryPropStorage is an in-memory dead-property store, port of sabre's
// PropertyStorage\Backend\Mock.
type MemoryPropStorage struct {
	// Props maps path → property name → value.
	Props map[string]map[xml.Name]any
}

func (m *MemoryPropStorage) ensure() {
	if m.Props == nil {
		m.Props = map[string]map[xml.Name]any{}
	}
}

func (m *MemoryPropStorage) PropFind(path string, pf *dav.PropFind) error {
	for name, value := range m.Props[path] {
		pf.Set(name, value, 200)
	}
	return nil
}

func (m *MemoryPropStorage) PropPatch(path string, pp *dav.PropPatch) error {
	m.ensure()
	pp.HandleRemaining(func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		if m.Props[path] == nil {
			m.Props[path] = map[xml.Name]any{}
		}
		for n, v := range values {
			if v == nil {
				delete(m.Props[path], n)
			} else {
				m.Props[path][n] = v
			}
		}
		return nil, true
	})
	return nil
}

func (m *MemoryPropStorage) Delete(path string) error {
	delete(m.Props, path)
	return nil
}

func (m *MemoryPropStorage) Move(source, destination string) error {
	m.ensure()
	if props, exists := m.Props[source]; exists {
		m.Props[destination] = props
		delete(m.Props, source)
	}
	return nil
}

// StreamingFile is a file whose size may be unknown (-1), like sabre's
// Mock\StreamingFile with a null size: no Content-Length, no ranges.
type StreamingFile struct {
	File
	size int64
}

// NewStreamingFile builds a streaming file; size -1 means unknown.
func NewStreamingFile(name, contents string, size int64) *StreamingFile {
	return &StreamingFile{File: *NewFile(name, contents), size: size}
}

func (s *StreamingFile) Size() int64 { return s.size }

// FailMode mirrors the failMode knob of sabre's Mock\PropertiesCollection,
// controlling how PropPatch mutations are answered.
type FailMode string

const (
	FailNone     FailMode = ""
	UpdateTrue   FailMode = "updatepropstrue"  // accept all mutations
	UpdateFalse  FailMode = "updatepropsfalse" // reject all mutations
	UpdateStatus FailMode = "updatepropsarray" // per-prop 402 statuses
)

// PropCollection is a collection carrying custom properties.
// Port of Sabre\DAV\Mock\PropertiesCollection.
type PropCollection struct {
	Collection
	Mode  FailMode
	props map[xml.Name]any
}

// NewPropCollection builds a properties collection.
func NewPropCollection(name string, props map[xml.Name]any, children ...dav.Node) *PropCollection {
	pc := &PropCollection{props: props}
	if pc.props == nil {
		pc.props = map[xml.Name]any{}
	}
	pc.Collection.name = name
	for _, child := range children {
		pc.Attach(child)
	}
	return pc
}

func (pc *PropCollection) Props(names []xml.Name) (map[xml.Name]any, error) {
	if len(names) == 0 {
		return pc.props, nil
	}
	out := map[xml.Name]any{}
	for _, n := range names {
		if v, exists := pc.props[n]; exists {
			out[n] = v
		}
	}
	return out, nil
}

func (pc *PropCollection) PropPatch(pp *dav.PropPatch) {
	pp.HandleRemaining(func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		switch pc.Mode {
		case UpdateFalse:
			return nil, false
		case UpdateStatus:
			statuses := make(map[xml.Name]int, len(values))
			for n := range values {
				statuses[n] = 402
			}
			return statuses, false
		default: // UpdateTrue and FailNone both accept
			for n, v := range values {
				if v == nil {
					delete(pc.props, n)
				} else {
					pc.props[n] = v
				}
			}
			return nil, true
		}
	})
}

// CreateDirectory mirrors sabre's override: children of a properties
// collection are properties collections with the same fail mode.
func (pc *PropCollection) CreateDirectory(name string) error {
	child := NewPropCollection(name, nil)
	child.Mode = pc.Mode
	pc.Attach(child)
	return nil
}
