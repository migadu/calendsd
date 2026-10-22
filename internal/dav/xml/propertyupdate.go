package davxml

import (
	"encoding/xml"
	"io"
	"strings"
)

// PropertyUpdate is a parsed PROPPATCH request body (RFC 4918 §9.2):
// mutations in document order. A nil value means removal; a string value is
// plain text content; []byte is a raw XML fragment.
type PropertyUpdate struct {
	// Names preserves document order (response propstats follow it).
	Names []xml.Name
	// Values maps property name to the new value (nil = remove).
	Values map[xml.Name]any
}

// ParsePropertyUpdate parses a PROPPATCH body with a token walk, so complex
// property values are re-serialized *self-contained* (every element carries
// its namespace as a default xmlns) — a stored fragment must not depend on
// prefixes declared by the request document.
func ParsePropertyUpdate(r io.Reader) (*PropertyUpdate, error) {
	dec := xml.NewDecoder(r)
	out := &PropertyUpdate{Values: map[xml.Name]any{}}

	// Walk: propertyupdate > (set|remove) > prop > *property elements*.
	var stack []xml.Name
	inRemove := false
	rootSeen := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if !rootSeen {
				return nil, io.ErrUnexpectedEOF
			}
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth := len(stack)
			name := t.Name
			isDav := name.Space == NSDav
			switch {
			case depth == 0 && (!isDav || name.Local != "propertyupdate"):
				return nil, xml.UnmarshalError("expected {DAV:}propertyupdate root")
			case depth == 0:
				rootSeen = true
			case depth == 1 && isDav && (name.Local == "set" || name.Local == "remove"):
				inRemove = name.Local == "remove"
			case depth == 3:
				// A property element inside set/remove > prop.
				value, err := parsePropValue(dec, t)
				if err != nil {
					return nil, err
				}
				if _, seen := out.Values[name]; !seen {
					out.Names = append(out.Names, name)
				}
				if inRemove {
					out.Values[name] = nil
				} else {
					out.Values[name] = value
				}
				continue // subtree fully consumed; no stack push
			}
			stack = append(stack, name)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
}

// parsePropValue consumes a property element's subtree. Text-only content
// returns a string; element content returns a self-contained XML fragment.
func parsePropValue(dec *xml.Decoder, start xml.StartElement) (any, error) {
	var text strings.Builder
	var fragment strings.Builder
	hasElements := false
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			hasElements = true
			depth++
			writeSelfContainedStart(&fragment, t)
		case xml.EndElement:
			if depth == 0 {
				// End of the property element itself.
				if hasElements {
					return []byte(fragment.String()), nil
				}
				return text.String(), nil
			}
			depth--
			fragment.WriteString("</" + t.Name.Local + ">")
		case xml.CharData:
			text.Write(t)
			fragment.WriteString(escapeText(string(t)))
		}
	}
}

// writeSelfContainedStart writes an element with its namespace declared as
// the default xmlns, making the fragment independent of outer prefixes.
func writeSelfContainedStart(b *strings.Builder, t xml.StartElement) {
	b.WriteString("<" + t.Name.Local)
	if t.Name.Space != "" {
		b.WriteString(` xmlns="` + escapeText(t.Name.Space) + `"`)
	}
	for _, attr := range t.Attr {
		if attr.Name.Space == "xmlns" || (attr.Name.Space == "" && attr.Name.Local == "xmlns") {
			continue // prefix declarations are re-derived
		}
		if attr.Name.Space != "" {
			// Namespaced attributes are rare in DAV bodies; qualify with
			// the full URI-in-clark form is not valid XML, so skip the
			// namespace (matching how sabre's fragment storage behaves
			// for undeclared attribute prefixes).
			b.WriteString(" " + attr.Name.Local + `="` + escapeText(attr.Value) + `"`)
			continue
		}
		b.WriteString(" " + attr.Name.Local + `="` + escapeText(attr.Value) + `"`)
	}
	b.WriteString(">")
}
