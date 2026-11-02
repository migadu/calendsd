package dav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
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

func (c *memColl) Name() string                                 { return c.name }
func (c *memColl) SetName(ctx context.Context, n string) error  { c.name = n; return nil }
func (c *memColl) LastModified() time.Time                      { return time.Time{} }
func (c *memColl) Children(ctx context.Context) ([]Node, error) { return c.children, nil }

func (c *memColl) Delete(ctx context.Context) error {
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

func (c *memColl) Child(ctx context.Context, name string) (Node, error) {
	for _, ch := range c.children {
		if ch.Name() == name {
			return ch, nil
		}
	}
	return nil, ErrNotFound("File not found: %s", name)
}

// childExists is ChildExists for tests: a lookup error fails the test.
func childExists(t testing.TB, c Collection, name string) bool {
	t.Helper()
	ok, err := c.ChildExists(t.Context(), name)
	if err != nil {
		t.Fatalf("ChildExists(%q): %v", name, err)
	}
	return ok
}

func (c *memColl) ChildExists(ctx context.Context, name string) (bool, error) {
	_, err := c.Child(ctx, name)
	return Found(err)
}

func (c *memColl) CreateFile(ctx context.Context, name string, data io.Reader) (string, error) {
	contents, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	c.children = append(c.children, &memFile{name: name, contents: contents, parent: c})
	return "", nil
}

func (c *memColl) CreateDirectory(ctx context.Context, name string) error {
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
	// seekable makes Get return a seekable body (BytesBody).
	seekable bool
}

func (f *memFile) Name() string                                { return f.name }
func (f *memFile) SetName(ctx context.Context, n string) error { f.name = n; return nil }
func (f *memFile) LastModified() time.Time                     { return f.mtime }
func (f *memFile) ContentType() string                         { return "" }
func (f *memFile) ETag() string                                { return f.etag }

func (f *memFile) Delete(ctx context.Context) error {
	if f.parent != nil {
		f.parent.removeChild(f.name)
	}
	return nil
}

func (f *memFile) Get(ctx context.Context) (io.ReadCloser, error) {
	if f.seekable {
		return BytesBody(f.contents), nil
	}
	return io.NopCloser(bytes.NewReader(f.contents)), nil
}

func (f *memFile) Put(ctx context.Context, data io.Reader) (string, error) {
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

func (s *memSyncColl) Changes(ctx context.Context, syncToken string, syncLevel, limit int) (*ChangeSet, error) {
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

func (u *unbindRecorder) AfterUnbind(ctx context.Context, path string) error {
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
	if childExists(t, coll, "..") {
		t.Errorf(`PUT created a node literally named ".." in the collection`)
	}
}

// An encoded "/" inside a segment survives the pre-decode Clean, so
// /coll/a%2F..%2F..%2Fx would reach the tree as the segment "a/../../x".
func TestPutRejectsEncodedSlashInSegment(t *testing.T) {
	coll := newMemColl("coll")
	root := newMemColl("root", coll)
	s := NewServer("/", root)

	req := httptest.NewRequest("PUT", "/coll/a%2F..%2F..%2Fx", strings.NewReader("evil"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("PUT with an encoded slash = %d, want 403", rec.Code)
	}
	if len(coll.children) != 0 || childExists(t, root, "x") {
		t.Errorf("PUT with an encoded slash created a node")
	}
}

func TestMoveOverwriteFiresUnbindHooksForDestination(t *testing.T) {
	src := &memFile{name: "src.txt", contents: []byte("source"), etag: `"s"`}
	dst := &memFile{name: "dst.txt", contents: []byte("dest"), etag: `"d"`}
	root := newMemColl("root", src, dst)
	s := NewServer("/", root)
	hooks := &unbindRecorder{}
	s.AddPlugin(hooks)

	req := httptest.NewRequest("MOVE", "/src.txt", nil)
	req.Header.Set("Destination", "/dst.txt")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("MOVE with overwrite = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if !slices.Contains(hooks.before, "dst.txt") {
		t.Errorf("BeforeUnbind not fired for overwritten MOVE destination (fired: %v)", hooks.before)
	}
}

// A stored object served with an ETag but no cache directive can be handed
// back stale by an intermediate/browser cache, and the stale ETag then fails
// the client's next If-Match PUT with a 412. GET and HEAD must carry a
// revalidate-forcing Cache-Control, plus no-transform so a proxy doesn't gzip
// the body and weaken the ETag (which likewise breaks the next If-Match).
func TestGetHeadSetNoCacheOnObjects(t *testing.T) {
	f := &memFile{name: "e.ics", contents: []byte("BEGIN:VCALENDAR"), etag: `"v1"`}
	root := newMemColl("root", f)
	s := NewServer("/", root)

	for _, method := range []string{"GET", "HEAD"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(method, "/e.ics", nil))
		if rec.Code != 200 {
			t.Fatalf("%s = %d: %s", method, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache, no-transform" {
			t.Errorf("%s Cache-Control = %q, want %q", method, cc, "private, no-cache, no-transform")
		}
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

// Browser hrefs are path-encoded, so a child name holding "?" or "#" links to
// the child instead of a query or fragment.
func TestBrowserEncodesHrefs(t *testing.T) {
	root := newMemColl("root", &memFile{name: "a?b#c.txt", contents: []byte("x")})
	s := NewServer("/", root)
	s.AddPlugin(&BrowserPlugin{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if body := rec.Body.String(); !strings.Contains(body, `href="/a%3Fb%23c.txt"`) {
		t.Errorf("browser href not path-encoded:\n%s", body)
	}
}

// A seekable body is served through http.ServeContent: ranges work, and an
// If-Range with a weak validator never matches (RFC 7233 §3.2 requires a
// strong comparison), so the full entity comes back.
func TestGetSeekableBodyServesRanges(t *testing.T) {
	file := &memFile{name: "f.txt", contents: []byte("0123456789"), etag: `"t"`, seekable: true}
	s := NewServer("/", newMemColl("root", file))

	get := func(hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/f.txt", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("Range", "bytes=2-4"); rec.Code != http.StatusPartialContent || rec.Body.String() != "234" ||
		rec.Header().Get("Content-Range") != "bytes 2-4/10" {
		t.Errorf("range GET = %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Range"))
	}
	if rec := get("Range", "bytes=2-4", "If-Range", `W/"t"`); rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Errorf("weak If-Range GET = %d %q, want the full entity", rec.Code, rec.Body.String())
	}
	if rec := get(); rec.Code != http.StatusOK || rec.Body.String() != "0123456789" || rec.Header().Get("ETag") != `"t"` {
		t.Errorf("plain GET = %d %q etag %q", rec.Code, rec.Body.String(), rec.Header().Get("ETag"))
	}
}

// A failed extended MKCOL lists its properties in a fixed order, not in map
// iteration order.
func TestExtendedMkcolFailureIsDeterministic(t *testing.T) {
	body := `<?xml version="1.0"?><d:mkcol xmlns:d="DAV:" xmlns:x="urn:x"><d:set><d:prop>` +
		`<d:resourcetype><d:collection/></d:resourcetype>` +
		`<x:e>1</x:e><x:c>1</x:c><x:a>1</x:a><x:d>1</x:d><x:b>1</x:b>` +
		`</d:prop></d:set></d:mkcol>`
	var first string
	for i := range 20 {
		s := NewServer("/", newMemColl("root"))
		req := httptest.NewRequest("MKCOL", "/new/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/xml")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMultiStatus {
			t.Fatalf("status = %d, want 207:\n%s", rec.Code, rec.Body.String())
		}
		got := rec.Body.String()
		if i == 0 {
			first = got
			if a, e := strings.Index(got, "<x1:a/>"), strings.Index(got, "<x1:e/>"); a < 0 || e < a {
				t.Fatalf("properties not sorted:\n%s", got)
			}
		} else if got != first {
			t.Fatalf("response differs between runs:\n%s\nvs\n%s", first, got)
		}
	}
}

// failingUnbind is an AfterUnbind listener that always fails.
type failingUnbind struct{}

func (failingUnbind) PluginName() string { return "failing-unbind" }
func (failingUnbind) Init(*Server)       {}
func (failingUnbind) AfterUnbind(context.Context, string) error {
	return errors.New("property cleanup failed")
}

// A failing after-unbind listener is logged, not dropped, and does not fail
// a DELETE that has already removed the node.
func TestAfterUnbindErrorIsLogged(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	root := newMemColl("root", &memFile{name: "f.txt", contents: []byte("x")})
	s := NewServer("/", root)
	s.AddPlugin(failingUnbind{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("DELETE", "/f.txt", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	if out := logs.String(); !strings.Contains(out, "after-unbind hook failed") ||
		!strings.Contains(out, "failing-unbind") || !strings.Contains(out, "property cleanup failed") {
		t.Errorf("hook error not logged; log:\n%s", out)
	}
}

// failingColl is a collection whose storage is down: every lookup fails.
type failingColl struct {
	memColl
	created bool
}

var errStorageDown = errors.New("storage down")

func (c *failingColl) Child(context.Context, string) (Node, error) { return nil, errStorageDown }

func (c *failingColl) ChildExists(context.Context, string) (bool, error) {
	return false, errStorageDown
}

func (c *failingColl) CreateFile(context.Context, string, io.Reader) (string, error) {
	c.created = true
	return "", nil
}

// A failed existence check must fail the request, not read as "absent": PUT
// would take the create path for a resource it could not look up.
func TestPutFailsWhenExistenceCheckFails(t *testing.T) {
	coll := &failingColl{memColl: memColl{name: "coll"}}
	s := NewServer("/", newMemColl("root", coll))

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("PUT", "/coll/x.txt", strings.NewReader("data")))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("PUT with a failing lookup = %d, want 500", rec.Code)
	}
	if coll.created {
		t.Error("PUT created the resource although its existence could not be checked")
	}
}

// Names reach Postgres text columns, which refuse invalid UTF-8 and NUL: such
// a path is the client's error, not a 500 from the storage lookup.
func TestPathRejectsInvalidUTF8AndNUL(t *testing.T) {
	s := NewServer("/", newMemColl("root"))
	for _, uri := range []string{"/%FF", "/a%00b"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("PROPFIND", uri, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PROPFIND %s = %d, want 400", uri, rec.Code)
		}
	}
}
