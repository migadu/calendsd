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

func TestParsePropfindRejectsTwoKinds(t *testing.T) {
	for _, body := range []string{
		`<D:propfind xmlns:D="DAV:"><D:allprop/><D:propname/></D:propfind>`,
		`<D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop><D:propname/></D:propfind>`,
		`<D:propfind xmlns:D="DAV:"><D:allprop/><D:prop><D:getetag/></D:prop></D:propfind>`,
	} {
		if _, err := ParsePropfind(strings.NewReader(body)); err == nil {
			t.Errorf("ParsePropfind accepted %s", body)
		}
	}
}
