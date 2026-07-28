package davxml

import (
	"bytes"
	"encoding/xml"
	"io"
	"testing"
)

// Bug: ParseMkcol stores element-valued properties as raw innerxml, keeping
// the request's namespace prefixes, so the stored fragment is not
// self-contained (ParsePropertyUpdate re-serializes with bound namespaces).
func TestParseMkcolElementValueIsSelfContained(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<D:mkcol xmlns:D="DAV:" xmlns:Z="urn:z">
 <D:set>
  <D:prop>
   <D:resourcetype><D:collection/></D:resourcetype>
   <Z:special><Z:thing/></Z:special>
  </D:prop>
 </D:set>
</D:mkcol>`)
	_, props, err := ParseMkcol(body)
	if err != nil {
		t.Fatalf("ParseMkcol: %v", err)
	}
	v, ok := props[xml.Name{Space: "urn:z", Local: "special"}]
	if !ok {
		t.Fatalf("missing {urn:z}special property, got %v", props)
	}
	frag, ok := v.([]byte)
	if !ok {
		t.Fatalf("property value = %T (%v), want a raw XML fragment", v, v)
	}

	// The stored fragment must be parseable standalone, with the child
	// element's prefix resolving to its namespace.
	dec := xml.NewDecoder(bytes.NewReader(frag))
	var root xml.Name
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			t.Fatalf("no element found in stored fragment %q", frag)
		}
		if err != nil {
			t.Fatalf("stored fragment is not standalone-parseable: %v\n%s", err, frag)
		}
		if se, isStart := tok.(xml.StartElement); isStart {
			root = se.Name
			break
		}
	}
	if (root != xml.Name{Space: "urn:z", Local: "thing"}) {
		t.Errorf("fragment root = {%s}%s, want {urn:z}thing — prefix unbound in stored fragment %q",
			root.Space, root.Local, frag)
	}
}
