package dav

// Audit 4.1: a percent-encoded slash survived the slash trim and path Clean
// (both run before decoding), so a Destination such as /%2Fsrc.txt decoded to
// a path the COPY/MOVE "same resource" guard did not recognise as the source
// while the tree still resolved it to the source: MOVE deleted the source and
// then failed. Encoded slashes are now refused outright (D6).

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEncodedSlashDestinationIsRefused(t *testing.T) {
	for _, method := range []string{"MOVE", "COPY"} {
		for _, dest := range []string{"/%2Fsrc.txt", "/src.txt%2F", "/%2Fsrc.txt%2F"} {
			src := &memFile{name: "src.txt", contents: []byte("source"), etag: `"s"`}
			root := newMemColl("root", src)
			s := NewServer("/", root)

			req := httptest.NewRequest(method, "/src.txt", nil)
			req.Header.Set("Destination", dest)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("%s to %q = %d, want 403: %s", method, dest, rec.Code, rec.Body.String())
			}
			if !childExists(t, root, "src.txt") {
				t.Errorf("%s to %q deleted the source", method, dest)
			}
		}
	}
}

func TestEncodedSlashRequestPathIsRefused(t *testing.T) {
	coll := newMemColl("coll", &memFile{name: "x.txt", contents: []byte("x"), etag: `"x"`})
	root := newMemColl("root", coll)
	s := NewServer("/", root)

	for _, path := range []string{"/coll%2F", "/%2Fcoll", "/coll%2Fx.txt"} {
		req := httptest.NewRequest("DELETE", path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("DELETE %s = %d, want 403: %s", path, rec.Code, rec.Body.String())
		}
	}
	if !childExists(t, root, "coll") || !childExists(t, coll, "x.txt") {
		t.Error("an encoded-slash DELETE removed a node")
	}
}
