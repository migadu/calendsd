package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
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
	nameFreeBusy      = xml.Name{Space: davxml.NSCalDAV, Local: "free-busy-query"}
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

// InterceptWrite implements dav.WriteInterceptor for calendar-object PUTs,
// mirroring sabre's validateICalendar: a jCal (RFC 7265) body is converted to
// iCalendar, and the object's component type is checked against the calendar's
// supported-calendar-component-set. Runs before the scheduling broker
// (registration order), so downstream sees validated iCalendar.
func (p *Plugin) InterceptWrite(r *http.Request, path string, data []byte, isNew bool) ([]byte, error) {
	parentPath, _ := splitPath(path)
	parent, err := p.server.Tree.NodeForPath(parentPath)
	if err != nil {
		return data, nil // not a calendar object write; leave it to other handlers
	}
	calNode, isCal := parent.(*Calendar)
	if !isCal {
		return data, nil
	}

	// jCal → iCalendar. Malformed jCal is rejected rather than stored raw.
	if ical.IsJCal(data) {
		decoded, err := ical.DecodeJCal(data)
		if err != nil {
			return nil, dav.ErrBadRequest("Invalid jCal request body: %s", err)
		}
		data = ical.EncodeLenient(decoded)
	}

	// A body that will not parse as iCalendar is rejected up front, the way
	// sabre's validateICalendar does, rather than handed to the backend, which
	// would surface the failure as an opaque 500.
	cal, err := ical.Decode(data)
	if err != nil {
		return nil, dav.ErrUnsupportedMediaType(
			"This resource only supports valid iCalendar 2.0 data. Parse error: %s", err)
	}
	compType := ical.ComponentType(cal)
	if compType == "" {
		return nil, invalidComponentType("iCalendar object must contain a VEVENT, VTODO or VJOURNAL component")
	}
	if allowed := calNode.SupportedComponents(); !slices.Contains(allowed, compType) {
		return nil, invalidComponentType(fmt.Sprintf(
			"This calendar only supports %s; the object is a %s", strings.Join(allowed, ", "), compType))
	}
	// The backend denormalizes every stored object (first/last occurrence,
	// recurrence bounds). Run that here so data which parses structurally but
	// is semantically invalid — a malformed DURATION, date, or RRULE — fails
	// as a 400 carrying the CalDAV valid-calendar-data precondition instead of
	// an opaque backend 500 leaking an internal error string.
	if _, err := ical.Denormalize(data); err != nil {
		return nil, invalidCalendarData("Invalid iCalendar data: %s", err)
	}
	return data, nil
}

// invalidCalendarData reports iCalendar that parses but is not usable (bad
// durations, dates, or recurrence rules) as a 400 carrying the RFC 4791
// {CALDAV:}valid-calendar-data precondition, mirroring sabre's precondition
// reporting rather than leaking an internal error as a 500.
func invalidCalendarData(format string, args ...any) error {
	e := dav.ErrBadRequest(format, args...)
	e.Precondition = []xml.Name{{Space: davxml.NSCalDAV, Local: "valid-calendar-data"}}
	return e
}

// invalidComponentType mirrors sabre's Sabre\CalDAV\Exception\InvalidComponentType
// (a 403 Forbidden subclass) so the error identity matches on the wire.
func invalidComponentType(msg string) error {
	return &dav.Error{
		Status:    http.StatusForbidden,
		Exception: `Sabre\CalDAV\Exception\InvalidComponentType`,
		Message:   msg,
	}
}

func (p *Plugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	switch r.Method {
	case "MKCALENDAR":
		return true, p.mkcalendar(w, r)
	case "GET":
		return p.handleExport(w, r)
	}
	return false, nil
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

	if _, isCal := node.(*Calendar); isCal {
		// Constant per-calendar properties sabre emits on every calendar
		// (max-resource-size is advertised, not enforced, matching sabre).
		pf.Handle(cal("max-resource-size"), func() any { return "10000000" })
		pf.Handle(cal("supported-calendar-data"), func() any {
			return davxml.Raw(`<cal:calendar-data content-type="text/calendar" version="2.0"/>`)
		})
		pf.Handle(cal("supported-collation-set"), func() any {
			return davxml.Raw(
				`<cal:supported-collation>i;ascii-casemap</cal:supported-collation>` +
					`<cal:supported-collation>i;octet</cal:supported-collation>` +
					`<cal:supported-collation>i;unicode-casemap</cal:supported-collation>`)
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
	case *Calendar:
		return []xml.Name{nameMultiget, nameCalendarQuery, nameFreeBusy}
	case *CalendarObject:
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
	case nameFreeBusy:
		return true, p.freeBusyQueryReport(body, path, w, r)
	}
	return false, nil
}

type freeBusyQueryXML struct {
	XMLName   xml.Name      `xml:"urn:ietf:params:xml:ns:caldav free-busy-query"`
	TimeRange *timeRangeXML `xml:"time-range"`
}

// PrivilegeChecker is the ACL enforcement surface the free-busy report needs
// (implemented by the acl plugin).
type PrivilegeChecker interface {
	CheckPrivileges(ctx context.Context, path string, privileges ...string) error
}

// freeBusyQueryReport implements the free-busy-query REPORT (RFC 4791
// §7.10), answering 200 with a text/calendar VFREEBUSY body like sabre.
func (p *Plugin) freeBusyQueryReport(body []byte, path string, w http.ResponseWriter, r *http.Request) error {
	var req freeBusyQueryXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	if req.TimeRange == nil {
		return dav.ErrBadRequest("The free-busy-query REPORT requires a time-range element")
	}
	tr, err := convertTimeRange(req.TimeRange)
	if err != nil {
		return err
	}
	start, end := tr.bounds()

	// Free-busy is gated by its own privilege ({DAV:}authenticated holds
	// it via the calendar ACL).
	for _, plugin := range p.server.Plugins() {
		if checker, ok := plugin.(PrivilegeChecker); ok {
			if err := checker.CheckPrivileges(r.Context(), path,
				"{urn:ietf:params:xml:ns:caldav}read-free-busy"); err != nil {
				return err
			}
		}
	}

	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return err
	}
	calNode, isCal := node.(*Calendar)
	if !isCal {
		return dav.ErrNotImplemented("The free-busy-query REPORT is only implemented on calendars")
	}

	objects, err := calNode.backend.CalendarObjects(calNode.Info.ID)
	if err != nil {
		return err
	}
	var payloads [][]byte
	for _, obj := range objects {
		payloads = append(payloads, obj.CalendarData)
	}
	result, err := ical.GenerateFreeBusy(payloads, start, end, p.calendarTimeZone(path), time.Now())
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/calendar")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(result)
	return err
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

	return p.writeObjectResponses(r.Context(), w, matches, query.Props, query.Expand, query.JSON, loc)
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
	return p.writeObjectResponses(r.Context(), w, paths, query.Props, query.Expand, query.JSON, loc)
}

// writeObjectResponses builds the multistatus for matched object paths,
// applying calendar-data expansion when requested.
func (p *Plugin) writeObjectResponses(ctx context.Context, w http.ResponseWriter, paths []string, props []xml.Name, expand *TimeRange, asJSON bool, loc *time.Location) error {
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
			if asJSON {
				if err := jsonResponseData(&resp); err != nil {
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

// jsonResponseData rewrites the calendar-data property of a response as
// jCal, for content-type="application/calendar+json" requests.
func jsonResponseData(resp *davxml.Response) error {
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
			cal, err := ical.Decode([]byte(ical.NormalizeCRLF(string(text))))
			if err != nil {
				return dav.ErrBadRequest("The calendar object could not be parsed: %s", err)
			}
			out, err := ical.JCal(cal)
			if err != nil {
				return err
			}
			ps.Props[i].Value = davxml.Text(out)
		}
	}
	return nil
}

// HandleGet implements dav.GetHandlerHook: calendar objects negotiate to
// jCal when the client asks for application/calendar+json (sabre's
// httpAfterGet transform).
func (p *Plugin) HandleGet(node dav.Node, w http.ResponseWriter, r *http.Request) (bool, error) {
	obj, isObject := node.(*CalendarObject)
	if !isObject || negotiateExportFormat(r.Header.Get("Accept")) != "application/calendar+json" {
		return false, nil
	}
	cal, err := ical.Decode(obj.Data())
	if err != nil {
		return true, dav.ErrBadRequest("The calendar object could not be parsed: %s", err)
	}
	body, err := ical.JCal(cal)
	if err != nil {
		return true, err
	}
	h := w.Header()
	h.Set("Content-Type", "application/calendar+json")
	if etag := obj.ETag(); etag != "" {
		h.Set("ETag", etag)
	}
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	return true, err
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
			cal, err := ical.Decode([]byte(ical.NormalizeCRLF(string(text))))
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
