package davxml

import (
	"bytes"
	"encoding/xml"
)

type mkcolXML struct {
	XMLName xml.Name `xml:"DAV: mkcol"`
	Set     struct {
		Prop struct {
			Props []struct {
				XMLName xml.Name
				Types   []struct {
					XMLName xml.Name
				} `xml:",any"`
				Value string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"prop"`
	} `xml:"set"`
}

// ParseMkcol extracts the {DAV:}resourcetype values plus any additional
// properties from an extended-mkcol body (RFC 5689). A body without
// resourcetype is an error, like sabre.
func ParseMkcol(body []byte) ([]xml.Name, map[xml.Name]any, error) {
	var parsed mkcolXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&parsed); err != nil {
		return nil, nil, err
	}
	var resourceType []xml.Name
	haveResourceType := false
	props := map[xml.Name]any{}
	for _, p := range parsed.Set.Prop.Props {
		if (p.XMLName == xml.Name{Space: NSDav, Local: "resourcetype"}) {
			haveResourceType = true
			for _, t := range p.Types {
				resourceType = append(resourceType, t.XMLName)
			}
			continue
		}
		props[p.XMLName] = p.Value
	}
	if !haveResourceType {
		return nil, nil, errMissingResourceType
	}
	return resourceType, props, nil
}

type resourceTypeError struct{}

func (resourceTypeError) Error() string {
	return "The mkcol request must include a {DAV:}resourcetype property"
}

var errMissingResourceType = resourceTypeError{}
