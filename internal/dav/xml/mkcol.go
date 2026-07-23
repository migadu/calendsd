package davxml

import (
	"bytes"
	"encoding/xml"
)

type mkcolXML struct {
	XMLName xml.Name `xml:"DAV: mkcol"`
	Set     struct {
		Prop struct {
			ResourceType *struct {
				Types []struct {
					XMLName xml.Name
				} `xml:",any"`
			} `xml:"resourcetype"`
		} `xml:"prop"`
	} `xml:"set"`
}

// ParseMkcolResourceType extracts the {DAV:}resourcetype values from an
// extended-mkcol body (RFC 5689). A body without resourcetype is an error,
// like sabre.
func ParseMkcolResourceType(body []byte) ([]xml.Name, error) {
	var parsed mkcolXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&parsed); err != nil {
		return nil, err
	}
	rt := parsed.Set.Prop.ResourceType
	if rt == nil {
		return nil, errMissingResourceType
	}
	out := make([]xml.Name, len(rt.Types))
	for i, t := range rt.Types {
		out[i] = t.XMLName
	}
	return out, nil
}

type resourceTypeError struct{}

func (resourceTypeError) Error() string {
	return "The mkcol request must include a {DAV:}resourcetype property"
}

var errMissingResourceType = resourceTypeError{}
