// MKCALENDAR handling (RFC 4791 §5.3.1) and its request-body parsing.

package caldav

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"

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

var (
	nameMkcalendar     = xml.Name{Space: davxml.NSCalDAV, Local: "mkcalendar"}
	namePropertyUpdate = xml.Name{Space: davxml.NSDav, Local: "propertyupdate"}
)

// parseMkcalendarProps reads the properties of an RFC 4791 §5.3.1 body
// (mkcalendar > set > prop > *). The body has the shape of a PROPPATCH body,
// so its tokens are re-encoded with the root renamed to {DAV:}propertyupdate
// and handed to davxml's namespace-aware parser: text values come back
// unescaped and element values as self-contained fragments that do not
// depend on prefixes declared by the request.
func parseMkcalendarProps(body []byte) (map[xml.Name]any, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if t.Name != nameMkcalendar {
					return nil, xml.UnmarshalError("expected {" + davxml.NSCalDAV + "}mkcalendar root")
				}
				t.Name = namePropertyUpdate
			}
			depth++
			// The decoder has resolved every name to its namespace, so the
			// prefix declarations are dropped; the encoder re-derives them.
			var attrs []xml.Attr
			for _, a := range t.Attr {
				if a.Name.Space != "xmlns" && a.Name != (xml.Name{Local: "xmlns"}) {
					attrs = append(attrs, a)
				}
			}
			t.Attr = attrs
			err = enc.EncodeToken(t)
		case xml.EndElement:
			depth--
			if depth == 0 {
				t.Name = namePropertyUpdate
			}
			err = enc.EncodeToken(t)
		case xml.CharData:
			err = enc.EncodeToken(t)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	update, err := davxml.ParsePropertyUpdate(&buf)
	if err != nil {
		return nil, err
	}
	out := map[xml.Name]any{}
	for name, value := range update.Values {
		if value != nil { // a <remove> has no place in MKCALENDAR
			out[name] = value
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
