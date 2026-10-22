package xmltest

import (
	"strings"
	"testing"
)

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
