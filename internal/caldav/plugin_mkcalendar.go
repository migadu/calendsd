// MKCALENDAR handling (RFC 4791 §5.3.1) and its request-body parsing.

package caldav

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"strings"

	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
	"calserver/internal/pathutil"
)

// mkcalendar creates a calendar collection (RFC 4791 §5.3.1).
func (p *Plugin) mkcalendar(w http.ResponseWriter, r *http.Request) error {
	path, err := p.server.Path(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	props := map[xml.Name]any{}
	if len(body) > 0 {
		parsed, err := parseMkcalendarProps(body)
		if err != nil {
			return dav.ErrBadRequest("%s", err)
		}
		props = parsed
	}

	if p.server.Tree.NodeExists(path) {
		return dav.ErrMethodNotAllowed("The resource you tried to create already exists")
	}
	parentPath, name := pathutil.Split(path)
	parent, err := p.server.Tree.NodeForPath(parentPath)
	if err != nil {
		return err
	}
	home, isHome := parent.(*CalendarHome)
	if !isHome {
		return dav.ErrMethodNotAllowed("You can only create calendars inside a calendar home")
	}
	// iCal abuses MKCALENDAR to create subscriptions: a resourcetype in
	// the body overrides the default, like sabre.
	resourceTypes := []xml.Name{
		{Space: davxml.NSDav, Local: "collection"}, nameCalendar,
	}
	if rt, hasRT := props[xml.Name{Space: davxml.NSDav, Local: "resourcetype"}]; hasRT {
		if raw, isRaw := rt.([]byte); isRaw {
			if names, err := parseResourceTypeFragment(raw); err == nil {
				resourceTypes = names
			}
		}
		delete(props, xml.Name{Space: davxml.NSDav, Local: "resourcetype"})
	}
	if err := home.CreateExtendedCollection(name, resourceTypes, props); err != nil {
		return err
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return nil
}

type mkcalendarXML struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:caldav mkcalendar"`
	Set     struct {
		Prop struct {
			Props []struct {
				XMLName xml.Name
				Inner   string `xml:",innerxml"`
			} `xml:",any"`
		} `xml:"prop"`
	} `xml:"DAV: set"`
}

func parseMkcalendarProps(body []byte) (map[xml.Name]any, error) {
	var parsed mkcalendarXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&parsed); err != nil {
		return nil, err
	}
	out := map[xml.Name]any{}
	for _, prop := range parsed.Set.Prop.Props {
		if strings.Contains(prop.Inner, "<") {
			out[prop.XMLName] = []byte(prop.Inner)
		} else {
			out[prop.XMLName] = prop.Inner
		}
	}
	return out, nil
}

// parseResourceTypeFragment extracts element names from a raw resourcetype
// fragment.
func parseResourceTypeFragment(raw []byte) ([]xml.Name, error) {
	dec := xml.NewDecoder(bytes.NewReader(append(append([]byte("<rt>"), raw...), []byte("</rt>")...)))
	var out []xml.Name
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth > 0 {
				out = append(out, t.Name)
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return out, nil
}
