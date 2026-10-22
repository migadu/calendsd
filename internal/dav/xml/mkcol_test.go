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

func TestParseMkcalendar(t *testing.T) {
	types, props, err := ParseMkcalendar([]byte(`<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
 <D:set><D:prop><D:displayname>Work &amp; Play</D:displayname></D:prop></D:set>
</C:mkcalendar>`))
	if err != nil || types != nil || props[xml.Name{Space: NSDav, Local: "displayname"}] != "Work & Play" {
		t.Errorf("plain body = %v, %v, %v; want no resourcetype and the unescaped displayname", types, props, err)
	}

	types, _, err = ParseMkcalendar([]byte(`<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:CS="http://calendarserver.org/ns/">
 <D:set><D:prop><D:resourcetype><D:collection/><CS:subscribed/></D:resourcetype></D:prop></D:set>
</C:mkcalendar>`))
	want := []xml.Name{{Space: NSDav, Local: "collection"}, {Space: "http://calendarserver.org/ns/", Local: "subscribed"}}
	if err != nil || len(types) != 2 || types[0] != want[0] || types[1] != want[1] {
		t.Errorf("resourcetype = %v, %v; want %v", types, err, want)
	}

	if _, _, err := ParseMkcalendar([]byte(`<D:mkcol xmlns:D="DAV:"/>`)); err == nil {
		t.Error("a {DAV:}mkcol root was accepted as MKCALENDAR")
	}
}

// Only set > prop sets properties; a remove block at creation is ignored.
func TestParseMkcalendarIgnoresRemove(t *testing.T) {
	_, props, err := ParseMkcalendar([]byte(`<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
 <D:remove><D:prop><D:displayname/></D:prop></D:remove>
</C:mkcalendar>`))
	if err != nil || len(props) != 0 {
		t.Errorf("remove block = %v, %v; want no properties", props, err)
	}
}
