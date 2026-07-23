package davxml

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Property is a single property inside a propstat: a qualified name plus its
// serialized content. Raw keeps unknown/dead properties round-trippable.
type Property struct {
	Name xml.Name
	// Text is the character-data content, used when Raw is nil.
	Text string
	// Raw is a pre-serialized XML fragment (element content only). Nested
	// elements inside it must use clark-unambiguous serialization via
	// WriteFragment; plain text may be given in Text instead.
	Raw []byte
}

// Propstat groups properties sharing one status code.
type Propstat struct {
	Status int
	Props  []Property
}

// Response is one {DAV:}response inside a multistatus.
type Response struct {
	Href      string
	Propstats []Propstat
	// Status is set for status-only responses (e.g. 404 in sync reports).
	Status int
}

// MultiStatus is a {DAV:}multistatus response document.
type MultiStatus struct {
	Responses []Response
	// SyncToken, when set, is emitted as {DAV:}sync-token (RFC 6578).
	SyncToken string
}

// prefixes assigns stable, sabre-like prefixes to well-known namespaces.
// Unknown namespaces get x1, x2, ... in first-use order.
var wellKnownPrefixes = map[string]string{
	NSDav:            "d",
	NSCalDAV:         "cal",
	NSCardDAV:        "card",
	NSCalendarServer: "cs",
	NSSabreDAV:       "s",
}

// Marshal serializes the multistatus document with an XML declaration,
// mirroring sabre's output shape (prefixed elements, namespaces declared on
// the root element).
func (ms *MultiStatus) Marshal() ([]byte, error) {
	// Collect namespaces in deterministic order: well-known first, then
	// others sorted.
	seen := map[string]bool{NSDav: true}
	var others []string
	collect := func(n xml.Name) {
		if n.Space != "" && !seen[n.Space] {
			seen[n.Space] = true
			others = append(others, n.Space)
		}
	}
	for _, r := range ms.Responses {
		for _, ps := range r.Propstats {
			for _, p := range ps.Props {
				collect(p.Name)
			}
		}
	}
	sort.Strings(others)

	prefix := map[string]string{NSDav: "d"}
	nextCustom := 1
	var nsAttrs []string
	nsAttrs = append(nsAttrs, `xmlns:d="DAV:"`)
	for _, ns := range others {
		p, ok := wellKnownPrefixes[ns]
		if !ok {
			p = fmt.Sprintf("x%d", nextCustom)
			nextCustom++
		}
		prefix[ns] = p
		nsAttrs = append(nsAttrs, fmt.Sprintf("xmlns:%s=%q", p, ns))
	}

	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<d:multistatus " + strings.Join(nsAttrs, " ") + ">\n")
	for _, r := range ms.Responses {
		writeResponse(&b, &r, prefix)
	}
	if ms.SyncToken != "" {
		fmt.Fprintf(&b, " <d:sync-token>%s</d:sync-token>\n", escapeText(ms.SyncToken))
	}
	b.WriteString("</d:multistatus>\n")
	return b.Bytes(), nil
}

func writeResponse(b *bytes.Buffer, r *Response, prefix map[string]string) {
	b.WriteString(" <d:response>\n")
	fmt.Fprintf(b, "  <d:href>%s</d:href>\n", escapeText(r.Href))
	for _, ps := range r.Propstats {
		b.WriteString("  <d:propstat>\n   <d:prop>\n")
		for _, p := range ps.Props {
			writeProperty(b, p, prefix)
		}
		fmt.Fprintf(b, "   </d:prop>\n   <d:status>HTTP/1.1 %s</d:status>\n  </d:propstat>\n", statusLine(ps.Status))
	}
	if r.Status != 0 {
		fmt.Fprintf(b, "  <d:status>HTTP/1.1 %s</d:status>\n", statusLine(r.Status))
	}
	b.WriteString(" </d:response>\n")
}

func writeProperty(b *bytes.Buffer, p Property, prefix map[string]string) {
	tag := qualify(p.Name, prefix)
	switch {
	case len(p.Raw) > 0:
		fmt.Fprintf(b, "    <%s>%s</%s>\n", tag, p.Raw, tag)
	case p.Text != "":
		fmt.Fprintf(b, "    <%s>%s</%s>\n", tag, escapeText(p.Text), tag)
	default:
		fmt.Fprintf(b, "    <%s/>\n", tag)
	}
}

func qualify(n xml.Name, prefix map[string]string) string {
	if p, ok := prefix[n.Space]; ok {
		return p + ":" + n.Local
	}
	return n.Local
}

// statusLine renders "200 OK" etc.; net/http's status texts agree with
// sabre/http on the codes multistatus bodies use.
func statusLine(code int) string {
	return fmt.Sprintf("%d %s", code, http.StatusText(code))
}

func escapeText(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
