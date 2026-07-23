package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

var (
	nameCalendarData  = xml.Name{Space: davxml.NSCalDAV, Local: "calendar-data"}
	nameCalendar      = xml.Name{Space: davxml.NSCalDAV, Local: "calendar"}
	nameCalendarQuery = xml.Name{Space: davxml.NSCalDAV, Local: "calendar-query"}
	nameMultiget      = xml.Name{Space: davxml.NSCalDAV, Local: "calendar-multiget"}
)

// Plugin implements the CalDAV protocol layer. Port of sabre's
// CalDAV\Plugin.
type Plugin struct {
	server *dav.Server
}

func (p *Plugin) PluginName() string { return "caldav" }

func (p *Plugin) Init(s *dav.Server) {
	p.server = s
	s.AddResourceTypeMapping(func(n dav.Node) []xml.Name {
		if _, isCal := n.(*Calendar); isCal {
			return []xml.Name{nameCalendar}
		}
		return nil
	})
}

func (p *Plugin) Features() []string { return []string{"calendar-access"} }

func (p *Plugin) Methods(path string) []string { return []string{"MKCALENDAR"} }

func (p *Plugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != "MKCALENDAR" {
		return false, nil
	}
	return true, p.mkcalendar(w, r)
}

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
	parentPath, name := splitPath(path)
	parent, err := p.server.Tree.NodeForPath(parentPath)
	if err != nil {
		return err
	}
	home, isHome := parent.(*CalendarHome)
	if !isHome {
		return dav.ErrMethodNotAllowed("You can only create calendars inside a calendar home")
	}
	if err := home.CreateCalendar(name, props); err != nil {
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

func splitPath(path string) (parent, base string) {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "", path
	}
	return path[:i], path[i+1:]
}

// PropFind implements dav.PropFindHook with the CalDAV properties.
func (p *Plugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	cal := func(local string) xml.Name { return xml.Name{Space: davxml.NSCalDAV, Local: local} }

	if ph, isPrincipal := node.(acl.PrincipalHolder); isPrincipal {
		pf.Handle(cal("calendar-home-set"), func() any {
			_, name := splitPath(ph.PrincipalURL())
			return davxml.Href{p.server.BaseURI + "calendars/" + name + "/"}
		})
	}

	if obj, isObject := node.(*CalendarObject); isObject {
		pf.Handle(nameCalendarData, func() any {
			// Sabre strips \r so the raw payload doesn't disturb XML
			// output.
			return strings.ReplaceAll(string(obj.Data()), "\r", "")
		})
	}
	return nil
}

// SupportedReports advertises the calendar reports on object containers.
func (p *Plugin) SupportedReports(node dav.Node, path string) []xml.Name {
	switch node.(type) {
	case *Calendar, *CalendarObject:
		return []xml.Name{nameMultiget, nameCalendarQuery}
	}
	return nil
}

// Report implements dav.ReportHandler.
func (p *Plugin) Report(name xml.Name, body []byte, path string, w http.ResponseWriter, r *http.Request) (bool, error) {
	switch name {
	case nameCalendarQuery:
		return true, p.calendarQueryReport(body, path, w, r)
	case nameMultiget:
		return true, p.calendarMultigetReport(body, w, r)
	}
	return false, nil
}

// calendarTimeZone resolves a calendar's timezone from its
// calendar-timezone property; UTC when unset, like sabre.
func (p *Plugin) calendarTimeZone(calendarPath string) *time.Location {
	node, err := p.server.Tree.NodeForPath(calendarPath)
	if err != nil {
		return time.UTC
	}
	calNode, isCal := node.(*Calendar)
	if !isCal {
		return time.UTC
	}
	tzProp, exists := calNode.Info.Props[xml.Name{Space: davxml.NSCalDAV, Local: "calendar-timezone"}]
	if !exists {
		return time.UTC
	}
	tzData, isString := tzProp.(string)
	if !isString {
		return time.UTC
	}
	vcal, err := ical.Decode([]byte(tzData))
	if err != nil {
		return time.UTC
	}
	for _, child := range vcal.Children {
		if child.Name == "VTIMEZONE" {
			if tzid := child.Props.Get("TZID"); tzid != nil {
				if loc, err := time.LoadLocation(tzid.Value); err == nil {
					return loc
				}
			}
		}
	}
	return time.UTC
}

func (p *Plugin) calendarQueryReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	query, err := ParseCalendarQuery(body)
	if err != nil {
		if _, isDAV := err.(*dav.Error); isDAV {
			return err
		}
		return dav.ErrBadRequest("%s", err)
	}
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return err
	}
	depth := 0
	if d := r.Header.Get("Depth"); d == "1" || strings.EqualFold(d, "infinity") {
		depth = 1
	}
	// Microsoft clients send Depth: 0 but mean 1, like sabre notes.
	if depth == 0 && strings.HasPrefix(r.Header.Get("User-Agent"), "MSFT-") {
		if _, isCal := node.(*Calendar); isCal {
			depth = 1
		}
	}

	loc := time.UTC
	if query.Expand != nil {
		calendarPath := path
		if _, isObj := node.(*CalendarObject); isObj {
			calendarPath, _ = splitPath(path)
		}
		loc = p.calendarTimeZone(calendarPath)
	}
	validator := &Validator{Loc: loc}

	var matches []string
	switch n := node.(type) {
	case *CalendarObject:
		if depth == 0 {
			ok, err := p.objectMatches(n, query, validator)
			if err != nil {
				return err
			}
			if ok {
				matches = append(matches, path)
			}
		}
	case *Calendar:
		if depth == 1 {
			children, err := n.Children()
			if err != nil {
				return err
			}
			for _, child := range children {
				obj := child.(*CalendarObject)
				ok, err := p.objectMatches(obj, query, validator)
				if err != nil {
					return err
				}
				if ok {
					matches = append(matches, path+"/"+obj.Name())
				}
			}
		}
	}

	return p.writeObjectResponses(r.Context(), w, matches, query.Props, query.Expand, loc)
}

func (p *Plugin) objectMatches(obj *CalendarObject, query *CalendarQuery, v *Validator) (bool, error) {
	cal, err := ical.Decode(obj.Data())
	if err != nil {
		return false, dav.ErrBadRequest("The calendar object could not be parsed: %s", err)
	}
	return v.Validate(cal, query.Filter)
}

func (p *Plugin) calendarMultigetReport(body []byte, w http.ResponseWriter, r *http.Request) error {
	query, err := ParseCalendarMultiget(body)
	if err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	var paths []string
	for _, href := range query.Hrefs {
		paths = append(paths, strings.Trim(strings.TrimPrefix(href, p.server.BaseURI), "/"))
	}
	loc := time.UTC
	if query.Expand != nil && len(paths) > 0 {
		calendarPath, _ := splitPath(paths[0])
		loc = p.calendarTimeZone(calendarPath)
	}
	return p.writeObjectResponses(r.Context(), w, paths, query.Props, query.Expand, loc)
}

// writeObjectResponses builds the multistatus for matched object paths,
// applying calendar-data expansion when requested.
func (p *Plugin) writeObjectResponses(ctx context.Context, w http.ResponseWriter, paths []string, props []xml.Name, expand *TimeRange, loc *time.Location) error {
	ms := &davxml.MultiStatus{}
	for _, objPath := range paths {
		responses, err := p.server.PropertiesForPath(ctx, objPath, props, 0)
		if err != nil {
			if dav.AsError(err).Status == http.StatusNotFound {
				ms.Responses = append(ms.Responses, davxml.Response{
					Href:   p.server.BaseURI + objPath,
					Status: http.StatusNotFound,
				})
				continue
			}
			return err
		}
		for _, resp := range responses {
			if expand != nil {
				if err := expandResponseData(&resp, expand, loc); err != nil {
					return err
				}
			}
			ms.Responses = append(ms.Responses, resp)
		}
	}

	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	h.Set("Vary", "Brief,Prefer")
	out, err := ms.Marshal()
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(out)
	return err
}

// expandResponseData rewrites the calendar-data property of a response with
// its recurrence-expanded form.
func expandResponseData(resp *davxml.Response, expand *TimeRange, loc *time.Location) error {
	for pi := range resp.Propstats {
		ps := &resp.Propstats[pi]
		if ps.Status != http.StatusOK {
			continue
		}
		for i, prop := range ps.Props {
			if prop.Name != nameCalendarData {
				continue
			}
			text, isText := prop.Value.(davxml.Text)
			if !isText {
				continue
			}
			// The propfind handler strips \r; restore CRLF for parsing.
			normalized := strings.ReplaceAll(strings.ReplaceAll(string(text), "\r\n", "\n"), "\n", "\r\n")
			cal, err := ical.Decode([]byte(normalized))
			if err != nil {
				return dav.ErrBadRequest("The calendar object could not be parsed: %s", err)
			}
			expanded, err := ical.Expand(cal, expand.Start, expand.End, loc)
			if err != nil {
				return err
			}
			ps.Props[i].Value = davxml.Text(ical.EncodeLenient(expanded))
		}
	}
	return nil
}
