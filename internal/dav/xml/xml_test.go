package davxml

import (
	"encoding/xml"
	"strings"
	"testing"
)

// applePropfind is the shape of PROPFIND Apple Calendar sends on principal
// discovery (captured shape; prefixes intentionally varied).
const applePropfind = `<?xml version="1.0" encoding="UTF-8"?>
<A:propfind xmlns:A="DAV:">
  <A:prop>
    <A:current-user-principal/>
    <A:principal-URL/>
    <B:calendar-home-set xmlns:B="urn:ietf:params:xml:ns:caldav"/>
    <A:displayname/>
  </A:prop>
</A:propfind>`

func TestParsePropfindNamed(t *testing.T) {
	pf, err := ParsePropfind(strings.NewReader(applePropfind))
	if err != nil {
		t.Fatalf("ParsePropfind: %v", err)
	}
	if pf.Type != PropfindProp {
		t.Fatalf("Type = %v, want PropfindProp", pf.Type)
	}
	want := []string{
		"{DAV:}current-user-principal",
		"{DAV:}principal-URL",
		"{urn:ietf:params:xml:ns:caldav}calendar-home-set",
		"{DAV:}displayname",
	}
	if len(pf.Props) != len(want) {
		t.Fatalf("got %d props, want %d: %v", len(pf.Props), len(want), pf.Props)
	}
	for i, n := range pf.Props {
		if Clark(n) != want[i] {
			t.Errorf("prop[%d] = %s, want %s", i, Clark(n), want[i])
		}
	}
}

func TestParsePropfindEmptyBodyIsAllprop(t *testing.T) {
	pf, err := ParsePropfind(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ParsePropfind: %v", err)
	}
	if pf.Type != PropfindAllProp {
		t.Errorf("Type = %v, want PropfindAllProp", pf.Type)
	}
}

func TestParsePropfindPropname(t *testing.T) {
	pf, err := ParsePropfind(strings.NewReader(
		`<propfind xmlns="DAV:"><propname/></propfind>`))
	if err != nil {
		t.Fatalf("ParsePropfind: %v", err)
	}
	if pf.Type != PropfindPropName {
		t.Errorf("Type = %v, want PropfindPropName", pf.Type)
	}
}

func TestClarkRoundTrip(t *testing.T) {
	n := xml.Name{Space: "urn:ietf:params:xml:ns:caldav", Local: "calendar-query"}
	got, err := ParseClark(Clark(n))
	if err != nil {
		t.Fatalf("ParseClark: %v", err)
	}
	if got != n {
		t.Errorf("round-trip changed name: %v -> %v", n, got)
	}
	if _, err := ParseClark("no-braces"); err == nil {
		t.Error("ParseClark should reject names without braces")
	}
}

// TestCanonicalizeEquivalence: same document, different prefixes, different
// attribute order, different response order, different formatting.
func TestCanonicalizeEquivalence(t *testing.T) {
	a := `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:cal="urn:ietf:params:xml:ns:caldav">
 <d:response>
  <d:href>/calendars/admin/work/</d:href>
  <d:propstat>
   <d:prop>
    <d:displayname>Work</d:displayname>
    <cal:supported-calendar-component-set>
     <cal:comp name="VEVENT"/><cal:comp name="VTODO"/>
    </cal:supported-calendar-component-set>
   </d:prop>
   <d:status>HTTP/1.1 200 OK</d:status>
  </d:propstat>
 </d:response>
 <d:response>
  <d:href>/calendars/admin/</d:href>
  <d:propstat><d:prop><d:displayname/></d:prop>
  <d:status>HTTP/1.1 404 Not Found</d:status></d:propstat>
 </d:response>
</d:multistatus>`

	// Same content: default DAV: namespace, responses swapped, prop order
	// swapped, attribute formatting differs.
	b := `<multistatus xmlns="DAV:"><response><href>/calendars/admin/</href>
<propstat><prop><displayname/></prop><status>HTTP/1.1 404 Not Found</status></propstat></response>
<response><href>/calendars/admin/work/</href><propstat><prop>
<C:supported-calendar-component-set xmlns:C="urn:ietf:params:xml:ns:caldav">
<C:comp name="VEVENT" /><C:comp name="VTODO" /></C:supported-calendar-component-set>
<displayname>Work</displayname>
</prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`

	ca, err := Canonicalize(strings.NewReader(a))
	if err != nil {
		t.Fatalf("Canonicalize(a): %v", err)
	}
	cb, err := Canonicalize(strings.NewReader(b))
	if err != nil {
		t.Fatalf("Canonicalize(b): %v", err)
	}
	if ca != cb {
		t.Errorf("canonical forms differ:\n--- a ---\n%s\n--- b ---\n%s", ca, cb)
	}
}

func TestCanonicalizeDetectsRealDifferences(t *testing.T) {
	base := `<d:prop xmlns:d="DAV:"><d:displayname>Work</d:displayname></d:prop>`
	for _, changed := range []string{
		`<d:prop xmlns:d="DAV:"><d:displayname>Home</d:displayname></d:prop>`,              // text
		`<d:prop xmlns:d="DAV:"><d:getetag>Work</d:getetag></d:prop>`,                      // name
		`<d:prop xmlns:x="X:" xmlns:d="DAV:"><x:displayname>Work</x:displayname></d:prop>`, // namespace
	} {
		cBase, err := Canonicalize(strings.NewReader(base))
		if err != nil {
			t.Fatal(err)
		}
		cChanged, err := Canonicalize(strings.NewReader(changed))
		if err != nil {
			t.Fatal(err)
		}
		if cBase == cChanged {
			t.Errorf("canonical form failed to distinguish:\n%s\nvs\n%s", base, changed)
		}
	}
}

// TestMultistatusMarshal: our writer's output must be semantically equal to
// the response sabre produces for the same PROPFIND (sabre-style reference
// handcrafted with sabre's prefixes and formatting).
func TestMultistatusMarshal(t *testing.T) {
	ms := &MultiStatus{
		Responses: []Response{
			{
				Href: "/calendars/admin/work/",
				Propstats: []Propstat{
					{Status: 200, Props: []Property{
						{Name: xml.Name{Space: NSDav, Local: "displayname"}, Value: Text("Work & Fun")},
						{Name: xml.Name{Space: NSCalDAV, Local: "calendar-timezone"}, Value: Text("UTC")},
					}},
					{Status: 404, Props: []Property{
						{Name: xml.Name{Space: NSDav, Local: "getcontentlength"}},
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

	cGot, err := Canonicalize(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("Canonicalize(ours): %v\n%s", err, out)
	}
	cWant, err := Canonicalize(strings.NewReader(sabreStyle))
	if err != nil {
		t.Fatalf("Canonicalize(sabre): %v", err)
	}
	if cGot != cWant {
		t.Errorf("multistatus not semantically equal to sabre reference:\n--- ours ---\n%s\n--- sabre ---\n%s\n--- raw ---\n%s", cGot, cWant, out)
	}
}
