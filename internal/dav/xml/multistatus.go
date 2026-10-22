package davxml

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Value is the serialized content of a property element.
type Value interface{ isValue() }

// Text is character-data content.
type Text string

// Raw is a pre-serialized XML fragment (element content only), used to
// round-trip unknown/dead properties verbatim.
type Raw []byte

// NameSet renders as a sequence of empty child elements, e.g. resourcetype's
// {DAV:}collection.
type NameSet []xml.Name

// Href renders as one {DAV:}href child element per entry, e.g.
// principal-URL or current-user-principal values.
type Href []string

// ReportSet renders as {DAV:}supported-report-set content: one
// supported-report/report wrapper per report name (RFC 3253 §3.1.5).
type ReportSet []xml.Name

// PrivilegeSet renders as {DAV:}current-user-privilege-set content: one
// {DAV:}privilege wrapper per privilege name (RFC 3744 §5.4).
type PrivilegeSet []xml.Name

// SupportedMethodSet renders as {DAV:}supported-method-set content: one
// {DAV:}supported-method element per method, named by its name attribute
// (RFC 3253 §3.1.3).
type SupportedMethodSet []string

// ResponseList renders nested {DAV:}response elements inside a property,
// used by expand-property to inline a referenced resource's properties.
type ResponseList []Response

// SupportedPrivilege is one node of a {DAV:}supported-privilege-set tree
// (RFC 3744 §5.3): a privilege name, an optional abstract flag, and nested
// aggregated privileges. Sabre emits no {DAV:}description, so neither do we.
type SupportedPrivilege struct {
	Name       xml.Name
	Abstract   bool
	Aggregates []SupportedPrivilege
}

// SupportedPrivilegeSet renders as {DAV:}supported-privilege-set content.
type SupportedPrivilegeSet []SupportedPrivilege

func (Text) isValue()                  {}
func (Raw) isValue()                   {}
func (NameSet) isValue()               {}
func (Href) isValue()                  {}
func (ReportSet) isValue()             {}
func (PrivilegeSet) isValue()          {}
func (SupportedMethodSet) isValue()    {}
func (ResponseList) isValue()          {}
func (SupportedPrivilegeSet) isValue() {}

// Property is a single property inside a propstat: a qualified name plus its
// content. A nil Value renders as an empty element.
type Property struct {
	Name  xml.Name
	Value Value
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
	var collectResponse func(r Response)
	collectResponse = func(r Response) {
		for _, ps := range r.Propstats {
			for _, p := range ps.Props {
				collect(p.Name)
				switch v := p.Value.(type) {
				case NameSet:
					for _, n := range v {
						collect(n)
					}
				case ReportSet:
					for _, n := range v {
						collect(n)
					}
				case PrivilegeSet:
					for _, n := range v {
						collect(n)
					}
				case SupportedPrivilegeSet:
					var walkPriv func(sp SupportedPrivilege)
					walkPriv = func(sp SupportedPrivilege) {
						collect(sp.Name)
						for _, sub := range sp.Aggregates {
							walkPriv(sub)
						}
					}
					for _, sp := range v {
						walkPriv(sp)
					}
				case ResponseList:
					for _, nested := range v {
						collectResponse(nested)
					}
				}
			}
		}
	}
	for _, r := range ms.Responses {
		collectResponse(r)
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
	// A status-only response (no propstats), like sabre's Response
	// serialization.
	if r.Status != 0 && len(r.Propstats) == 0 {
		fmt.Fprintf(b, "  <d:status>HTTP/1.1 %s</d:status>\n", statusLine(r.Status))
	}
	b.WriteString(" </d:response>\n")
}

func writeProperty(b *bytes.Buffer, p Property, prefix map[string]string) {
	tag := qualify(p.Name, prefix)
	switch v := p.Value.(type) {
	case Raw:
		if len(v) == 0 {
			fmt.Fprintf(b, "    <%s/>\n", tag)
			return
		}
		fmt.Fprintf(b, "    <%s>%s</%s>\n", tag, v, tag)
	case Text:
		if v == "" {
			fmt.Fprintf(b, "    <%s/>\n", tag)
			return
		}
		fmt.Fprintf(b, "    <%s>%s</%s>\n", tag, escapeText(string(v)), tag)
	case NameSet:
		if len(v) == 0 {
			fmt.Fprintf(b, "    <%s/>\n", tag)
			return
		}
		fmt.Fprintf(b, "    <%s>", tag)
		for _, n := range v {
			fmt.Fprintf(b, "<%s/>", qualify(n, prefix))
		}
		fmt.Fprintf(b, "</%s>\n", tag)
	case Href:
		if len(v) == 0 {
			fmt.Fprintf(b, "    <%s/>\n", tag)
			return
		}
		fmt.Fprintf(b, "    <%s>", tag)
		for _, href := range v {
			fmt.Fprintf(b, "<d:href>%s</d:href>", escapeText(href))
		}
		fmt.Fprintf(b, "</%s>\n", tag)
	case ReportSet:
		fmt.Fprintf(b, "    <%s>", tag)
		for _, n := range v {
			fmt.Fprintf(b, "<d:supported-report><d:report><%s/></d:report></d:supported-report>", qualify(n, prefix))
		}
		fmt.Fprintf(b, "</%s>\n", tag)
	case PrivilegeSet:
		fmt.Fprintf(b, "    <%s>", tag)
		for _, n := range v {
			fmt.Fprintf(b, "<d:privilege><%s/></d:privilege>", qualify(n, prefix))
		}
		fmt.Fprintf(b, "</%s>\n", tag)
	case SupportedMethodSet:
		fmt.Fprintf(b, "    <%s>", tag)
		for _, m := range v {
			fmt.Fprintf(b, `<d:supported-method name="%s"/>`, escapeText(m))
		}
		fmt.Fprintf(b, "</%s>\n", tag)
	case SupportedPrivilegeSet:
		fmt.Fprintf(b, "    <%s>", tag)
		for _, sp := range v {
			writeSupportedPrivilege(b, sp, prefix)
		}
		fmt.Fprintf(b, "</%s>\n", tag)
	case ResponseList:
		if len(v) == 0 {
			fmt.Fprintf(b, "    <%s/>\n", tag)
			return
		}
		fmt.Fprintf(b, "    <%s>\n", tag)
		for i := range v {
			writeResponse(b, &v[i], prefix)
		}
		fmt.Fprintf(b, "    </%s>\n", tag)
	default:
		fmt.Fprintf(b, "    <%s/>\n", tag)
	}
}

// writeSupportedPrivilege renders one privilege subtree, mirroring sabre's
// SupportedPrivilegeSet::serializePriv (privilege, optional abstract, then
// nested supported-privilege children; no description, like sabre).
func writeSupportedPrivilege(b *bytes.Buffer, sp SupportedPrivilege, prefix map[string]string) {
	b.WriteString("<d:supported-privilege>")
	fmt.Fprintf(b, "<d:privilege><%s/></d:privilege>", qualify(sp.Name, prefix))
	if sp.Abstract {
		b.WriteString("<d:abstract/>")
	}
	for _, sub := range sp.Aggregates {
		writeSupportedPrivilege(b, sub, prefix)
	}
	b.WriteString("</d:supported-privilege>")
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

// escapeText escapes s for XML character data and attribute values alike
// (xml.EscapeText escapes quotes too). It is this package's only escaper.
func escapeText(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
