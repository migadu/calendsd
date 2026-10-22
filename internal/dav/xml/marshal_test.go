package davxml_test

import (
	"encoding/xml"
	"strings"
	"testing"

	davxml "calendsd/internal/dav/xml"
	"calendsd/internal/dav/xml/xmltest"
)

// TestMultistatusMarshal: our writer's output must be semantically equal to
// the response sabre produces for the same PROPFIND (sabre-style reference
// handcrafted with sabre's prefixes and formatting).
func TestMultistatusMarshal(t *testing.T) {
	ms := &davxml.MultiStatus{
		Responses: []davxml.Response{
			{
				Href: "/calendars/admin/work/",
				Propstats: []davxml.Propstat{
					{Status: 200, Props: []davxml.Property{
						{Name: xml.Name{Space: davxml.NSDav, Local: "displayname"}, Value: davxml.Text("Work & Fun")},
						{Name: xml.Name{Space: davxml.NSCalDAV, Local: "calendar-timezone"}, Value: davxml.Text("UTC")},
					}},
					{Status: 404, Props: []davxml.Property{
						{Name: xml.Name{Space: davxml.NSDav, Local: "getcontentlength"}},
					}},
				},
			},
		},
	}
	out, err := ms.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	sabreStyle := `<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:cal="urn:ietf:params:xml:ns:caldav">
 <d:response>
  <d:href>/calendars/admin/work/</d:href>
  <d:propstat>
   <d:prop>
    <d:displayname>Work &amp; Fun</d:displayname>
    <cal:calendar-timezone>UTC</cal:calendar-timezone>
   </d:prop>
   <d:status>HTTP/1.1 200 OK</d:status>
  </d:propstat>
  <d:propstat>
   <d:prop><d:getcontentlength/></d:prop>
   <d:status>HTTP/1.1 404 Not Found</d:status>
  </d:propstat>
 </d:response>
</d:multistatus>`

	cGot, err := xmltest.Canonicalize(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("xmltest.Canonicalize(ours): %v\n%s", err, out)
	}
	cWant, err := xmltest.Canonicalize(strings.NewReader(sabreStyle))
	if err != nil {
		t.Fatalf("xmltest.Canonicalize(sabre): %v", err)
	}
	if cGot != cWant {
		t.Errorf("multistatus not semantically equal to sabre reference:\n--- ours ---\n%s\n--- sabre ---\n%s\n--- raw ---\n%s", cGot, cWant, out)
	}
}
