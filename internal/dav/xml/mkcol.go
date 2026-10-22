package davxml

import (
	"bytes"
	"encoding/xml"
	"io"
)

// ParseMkcol extracts the {DAV:}resourcetype values plus any additional
// properties from an extended-mkcol body (RFC 5689). A body without
// resourcetype is an error, like sabre.
//
// Like ParsePropertyUpdate, it uses a token walk so element-valued properties
// are re-serialized *self-contained* — a stored fragment must not depend on
// prefixes declared by the request document.
func ParseMkcol(body []byte) ([]xml.Name, map[xml.Name]any, error) {
	resourceType, haveResourceType, props, err := parseCreateBody(body, xml.Name{Space: NSDav, Local: "mkcol"})
	if err != nil {
		return nil, nil, err
	}
	if !haveResourceType {
		return nil, nil, errMissingResourceType
	}
	return resourceType, props, nil
}

// ParseMkcalendar extracts the properties of an MKCALENDAR body (RFC 4791
// §5.3.1), which has the extended-mkcol shape under a {CalDAV}mkcalendar
// root. resourceType is nil when the body sets none (the caller's default
// applies); iCal sets one to create subscriptions.
func ParseMkcalendar(body []byte) (resourceType []xml.Name, props map[xml.Name]any, err error) {
	resourceType, _, props, err = parseCreateBody(body, xml.Name{Space: NSCalDAV, Local: "mkcalendar"})
	return resourceType, props, err
}

// parseCreateBody walks root > set > prop > *property elements*, the shape
// extended MKCOL and MKCALENDAR share.
func parseCreateBody(body []byte, root xml.Name) (resourceType []xml.Name, haveResourceType bool, props map[xml.Name]any, err error) {
	dec := xml.NewDecoder(bytes.NewReader(body))

	var stack []xml.Name
	props = map[xml.Name]any{}
	rootSeen := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if !rootSeen {
				return nil, false, nil, io.ErrUnexpectedEOF
			}
			break
		}
		if err != nil {
			return nil, false, nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth := len(stack)
			name := t.Name
			switch {
			case depth == 0 && name != root:
				return nil, false, nil, xml.UnmarshalError("expected " + Clark(root) + " root")
			case depth == 0:
				rootSeen = true
			case depth == 3 && stack[1] == xml.Name{Space: NSDav, Local: "set"} && stack[2] == xml.Name{Space: NSDav, Local: "prop"}:
				// A property element inside set > prop. Anything else at this
				// depth (a <remove>, which has no meaning at creation) is
				// walked past like any other element.
				if (name == xml.Name{Space: NSDav, Local: "resourcetype"}) {
					haveResourceType = true
					types, err := parseResourceType(dec)
					if err != nil {
						return nil, false, nil, err
					}
					resourceType = append(resourceType, types...)
					continue // subtree fully consumed; no stack push
				}
				value, err := parsePropValue(dec, t)
				if err != nil {
					return nil, false, nil, err
				}
				props[name] = value
				continue // subtree fully consumed; no stack push
			}
			stack = append(stack, name)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return resourceType, haveResourceType, props, nil
}

// parseResourceType consumes a {DAV:}resourcetype subtree, returning the names
// of its immediate child elements (e.g. {DAV:}collection).
func parseResourceType(dec *xml.Decoder) ([]xml.Name, error) {
	var types []xml.Name
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				types = append(types, t.Name)
			}
			depth++
		case xml.EndElement:
			if depth == 0 {
				return types, nil
			}
			depth--
		}
	}
}

type resourceTypeError struct{}

func (resourceTypeError) Error() string {
	return "The mkcol request must include a {DAV:}resourcetype property"
}

var errMissingResourceType = resourceTypeError{}
