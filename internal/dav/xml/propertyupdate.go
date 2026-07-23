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

type rawProp struct {
	XMLName xml.Name
	Inner   string `xml:",innerxml"`
}

type propertyUpdateXML struct {
	XMLName xml.Name `xml:"DAV: propertyupdate"`
	Ops     []struct {
		XMLName xml.Name
		Prop    struct {
			Props []rawProp `xml:",any"`
		} `xml:"prop"`
	} `xml:",any"`
}

// ParsePropertyUpdate parses a PROPPATCH body.
func ParsePropertyUpdate(r io.Reader) (*PropertyUpdate, error) {
	var body propertyUpdateXML
	if err := xml.NewDecoder(r).Decode(&body); err != nil {
		return nil, err
	}
	out := &PropertyUpdate{Values: map[xml.Name]any{}}
	for _, op := range body.Ops {
		isRemove := op.XMLName == xml.Name{Space: NSDav, Local: "remove"}
		isSet := op.XMLName == xml.Name{Space: NSDav, Local: "set"}
		if !isRemove && !isSet {
			continue
		}
		for _, p := range op.Prop.Props {
			if _, seen := out.Values[p.XMLName]; !seen {
				out.Names = append(out.Names, p.XMLName)
			}
			if isRemove {
				out.Values[p.XMLName] = nil
				continue
			}
			out.Values[p.XMLName] = propValue(p.Inner)
		}
	}
	return out, nil
}

// propValue distinguishes plain text content from nested XML: fragments with
// child elements stay raw, everything else is unescaped text.
func propValue(inner string) any {
	if strings.Contains(inner, "<") {
		return []byte(inner)
	}
	var text string
	// Round-trip through the decoder to resolve entities (&amp; etc.).
	if err := xml.Unmarshal([]byte("<v>"+inner+"</v>"), &text); err != nil {
		return inner
	}
	return text
}
