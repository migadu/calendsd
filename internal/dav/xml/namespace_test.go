package davxml

import (
	"encoding/xml"
	"strings"
	"testing"
)

// A client-chosen property namespace is echoed into an xmlns attribute; it
// must be attribute-escaped, not %q-quoted (audit 6.2).
func TestMultiStatusEscapesNamespaceAttribute(t *testing.T) {
	ms := &MultiStatus{Responses: []Response{{
		Href: "/x",
		Propstats: []Propstat{{Status: 404, Props: []Property{
			{Name: xml.Name{Space: "urn:a&b<c\"d", Local: "p"}},
			{Name: xml.Name{Space: "urn:ümlaut", Local: "q"}},
		}}},
	}}}
	out, err := ms.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `="urn:a&amp;b&lt;c&#34;d"`) && !strings.Contains(s, `="urn:a&amp;b&lt;c&quot;d"`) {
		t.Errorf("namespace not attribute-escaped:\n%s", s)
	}
	if !strings.Contains(s, `="urn:ümlaut"`) {
		t.Errorf("non-ASCII namespace was rewritten:\n%s", s)
	}
	if err := xml.Unmarshal(out, new(struct{})); err != nil {
		t.Errorf("output is not well-formed XML: %v\n%s", err, s)
	}
}
