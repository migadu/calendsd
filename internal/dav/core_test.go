package dav

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// In-memory fixtures for core-plugin tests (the mock package can't be
// imported here without a cycle; see testRoot in server_test.go).

type memColl struct {
	name     string
	parent   *memColl
	children []Node
}

func newMemColl(name string, children ...Node) *memColl {
	c := &memColl{name: name}
	for _, ch := range children {
		c.attach(ch)
	}
	return c
}

func (c *memColl) attach(n Node) {
	switch v := n.(type) {
	case *memColl:
		v.parent = c
	case *memFile:
		v.parent = c
	case *memSyncColl:
		v.memColl.parent = c
	}
	c.children = append(c.children, n)
}

func (c *memColl) Name() string             { return c.name }
func (c *memColl) SetName(n string) error   { c.name = n; return nil }
func (c *memColl) LastModified() time.Time  { return time.Time{} }
func (c *memColl) Children() ([]Node, error) { return c.children, nil }

func (c *memColl) Delete() error {
	if c.parent != nil {
		c.parent.removeChild(c.name)
	}
	return nil
}

func (c *memColl) removeChild(name string) {
	for i, ch := range c.children {
		if ch.Name() == name {
			c.children = append(c.children[:i], c.children[i+1:]...)
			return
		}
	}
}

func (c *memColl) Child(name string) (Node, error) {
	for _, ch := range c.children {
		if ch.Name() == name {
			return ch, nil
		}
	}
	return nil, ErrNotFound("File not found: %s", name)
}

func (c *memColl) ChildExists(name string) bool {
	_, err := c.Child(name)
	return err == nil
}

func (c *memColl) CreateFile(name string, data io.Reader) (string, error) {
	contents, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	c.children = append(c.children, &memFile{name: name, contents: contents, parent: c})
	return "", nil
}

func (c *memColl) CreateDirectory(name string) error {
	c.children = append(c.children, &memColl{name: name, parent: c})
	return nil
}

type memFile struct {
	name     string
	contents []byte
	etag     string
	mtime    time.Time
	parent   *memColl
	// sizeOverride, when set, is returned by Size (use -1 for "unknown").
	sizeOverride *int64
}

func (f *memFile) Name() string            { return f.name }
func (f *memFile) SetName(n string) error  { f.name = n; return nil }
func (f *memFile) LastModified() time.Time { return f.mtime }
func (f *memFile) ContentType() string     { return "" }
func (f *memFile) ETag() string            { return f.etag }

func (f *memFile) Delete() error {
	if f.parent != nil {
		f.parent.removeChild(f.name)
	}
	return nil
}

func (f *memFile) Get() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.contents)), nil
}

func (f *memFile) Put(data io.Reader) (string, error) {
	contents, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	f.contents = contents
	return f.etag, nil
}

func (f *memFile) Size() int64 {
	if f.sizeOverride != nil {
		return *f.sizeOverride
	}
	return int64(len(f.contents))
}

// memSyncColl is a minimal RFC 6578 sync-capable collection.
type memSyncColl struct {
	memColl
	token string
}

func (s *memSyncColl) SyncToken() string { return s.token }

func (s *memSyncColl) Changes(syncToken string, syncLevel, limit int) (*ChangeSet, error) {
	if syncToken != "" && syncToken != s.token {
		return nil, nil // invalid/unknown token
	}
	return &ChangeSet{SyncToken: s.token}, nil
}

// unbindRecorder records BeforeUnbind/AfterUnbind hook invocations.
type unbindRecorder struct {
	before, after []string
}

func (u *unbindRecorder) PluginName() string { return "unbind-recorder" }
func (u *unbindRecorder) Init(*Server)       {}

func (u *unbindRecorder) BeforeUnbind(r *http.Request, path string) error {
	u.before = append(u.before, path)
	return nil
}

func (u *unbindRecorder) AfterUnbind(path string) error {
	u.after = append(u.after, path)
	return nil
}

// Bug: core.go propfind drops ParsePropfind's Include list, so PROPFIND
// allprop with <include> never returns the included properties (RFC 4918 §9.1).
func TestPropfindAllpropIncludeReturnsIncludedProps(t *testing.T) {
	sc := &memSyncColl{memColl: memColl{name: "cal"}, token: "1"}
	root := newMemColl("root", sc)
	s := NewServer("/", root)
	s.AddPlugin(&SyncPlugin{})

	body := `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:">
 <d:allprop/>
 <d:include><d:sync-token/></d:include>
</d:propfind>`
	req := httptest.NewRequest("PROPFIND", "/cal/", strings.NewReader(body))
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != 207 {
		t.Fatalf("PROPFIND = %d: %s", rec.Code, rec.Body.String())
	}
	if out := rec.Body.String(); !strings.Contains(out, SyncTokenPrefix+"1") {
		t.Errorf("allprop+include response missing the included {DAV:}sync-token property:\n%s", out)
	}
}

// Bug: core.go copy() deletes an overwritten destination without firing the
// BeforeUnbind/AfterUnbind hooks that DELETE and MOVE fire for the same removal.
func TestCopyOverwriteFiresUnbindHooksForDestination(t *testing.T) {
	src := &memFile{name: "src.txt", contents: []byte("source"), etag: `"s"`}
	dst := &memFile{name: "dst.txt", contents: []byte("dest"), etag: `"d"`}
	root := newMemColl("root", src, dst)
	s := NewServer("/", root)
	hooks := &unbindRecorder{}
	s.AddPlugin(hooks)

	req := httptest.NewRequest("COPY", "/src.txt", nil)
	req.Header.Set("Destination", "/dst.txt")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("COPY with overwrite = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if !slices.Contains(hooks.before, "dst.txt") {
		t.Errorf("BeforeUnbind not fired for overwritten COPY destination (fired: %v)", hooks.before)
	}
	if !slices.Contains(hooks.after, "dst.txt") {
		t.Errorf("AfterUnbind not fired for overwritten COPY destination (fired: %v)", hooks.after)
	}
}

// Bug: core.go options() passes the raw r.URL.Path (BaseURI still included) to
// Methods()/NodeExists, so existing resources under a non-root BaseURI look
// unmapped and MKCOL is wrongly advertised (sabre only advertises MKCOL on
// unmapped URIs).
func TestOptionsExistingPathUnderBaseURIDoesNotAdvertiseMkcol(t *testing.T) {
	root := newMemColl("root", newMemColl("coll"))
	s := NewServer("/dav/", root)

	req := httptest.NewRequest("OPTIONS", "/dav/coll", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS = %d, want 200", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); strings.Contains(allow, "MKCOL") {
		t.Errorf("Allow header %q advertises MKCOL for an existing resource under BaseURI /dav/", allow)
	}
}

// Bug: percent-encoded dot segments survive server.go's path Clean (which runs
// before decoding), so PUT /coll/%2e%2e reaches the tree as a literal ".."
// segment and creates a node named "..".
func TestPutRejectsPercentEncodedDotDotSegment(t *testing.T) {
	coll := newMemColl("coll")
	root := newMemColl("root", coll)
	s := NewServer("/", root)

	req := httptest.NewRequest("PUT", "/coll/%2e%2e", strings.NewReader("evil"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code < 400 {
		t.Errorf("PUT /coll/%%2e%%2e = %d, want a 4xx rejection", rec.Code)
	}
	if coll.ChildExists("..") {
		t.Errorf(`PUT created a node literally named ".." in the collection`)
	}
}

// Bug: core.go's getcontentlength handler serializes file.Size() verbatim, so
// unknown-size files (-1, treated as unknown by GET) report "-1"; sabre reports
// the property as 404 instead.
func TestPropfindGetcontentlengthUnknownSizeIs404(t *testing.T) {
	unknown := int64(-1)
	f := &memFile{name: "stream.txt", contents: []byte("streamed"), etag: `"x"`, sizeOverride: &unknown}
	root := newMemColl("root", f)
	s := NewServer("/", root)

	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getcontentlength/></d:prop></d:propfind>`
	req := httptest.NewRequest("PROPFIND", "/stream.txt", strings.NewReader(body))
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != 207 {
		t.Fatalf("PROPFIND = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, "-1") {
		t.Errorf("getcontentlength serialized as -1 for an unknown-size file:\n%s", out)
	}
	if !strings.Contains(out, "<d:getcontentlength/>") || !strings.Contains(out, "404 Not Found") {
		t.Errorf("getcontentlength for an unknown-size file must be a 404 propstat (sabre parity):\n%s", out)
	}
}
