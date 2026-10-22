package davxml

import (
	"encoding/xml"
	"errors"
	"io"
)

// PropfindType distinguishes the three PROPFIND request flavors (RFC 4918
// §9.1): a named property list, allprop, or propname.
type PropfindType int

const (
	PropfindProp PropfindType = iota
	PropfindAllProp
	PropfindPropName
)

// Propfind is a parsed PROPFIND request body.
type Propfind struct {
	Type PropfindType
	// Props lists the requested properties when Type is PropfindProp.
	Props []xml.Name
	// Include lists additional properties requested next to allprop.
	Include []xml.Name
}

type anyName struct {
	XMLName xml.Name
}

type propfindXML struct {
	XMLName xml.Name `xml:"DAV: propfind"`
	Prop    *struct {
		Names []anyName `xml:",any"`
	} `xml:"prop"`
	AllProp  *struct{} `xml:"allprop"`
	PropName *struct{} `xml:"propname"`
	Include  *struct {
		Names []anyName `xml:",any"`
	} `xml:"include"`
}

// ParsePropfind parses a PROPFIND request body. An empty body means allprop
// (RFC 4918 §9.1), matching sabre's behavior.
func ParsePropfind(r io.Reader) (*Propfind, error) {
	var body propfindXML
	err := xml.NewDecoder(r).Decode(&body)
	if err == io.EOF {
		return &Propfind{Type: PropfindAllProp}, nil
	}
	if err != nil {
		return nil, err
	}

	names := func(in *struct {
		Names []anyName `xml:",any"`
	}) []xml.Name {
		if in == nil {
			return nil
		}
		out := make([]xml.Name, len(in.Names))
		for i, n := range in.Names {
			out[i] = n.XMLName
		}
		return out
	}

	// RFC 4918 §14.20: propfind is exactly one of propname, allprop (with an
	// optional include) or prop.
	chosen := 0
	for _, set := range []bool{body.PropName != nil, body.AllProp != nil, body.Prop != nil} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		return nil, errors.New("propfind body must contain only one of prop, allprop or propname")
	}
	switch {
	case body.PropName != nil:
		return &Propfind{Type: PropfindPropName}, nil
	case body.AllProp != nil:
		return &Propfind{Type: PropfindAllProp, Include: names(body.Include)}, nil
	case body.Prop != nil:
		return &Propfind{Type: PropfindProp, Props: names(body.Prop)}, nil
	default:
		return nil, errors.New("propfind body must contain prop, allprop or propname")
	}
}
