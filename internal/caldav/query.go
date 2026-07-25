package caldav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"

	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
)

// maxFilterDepth caps element nesting in a REPORT filter body. Real CalDAV
// filters nest a handful of levels (VCALENDAR > VEVENT > VALARM); this bound is
// generous while stopping a deeply-nested body from exhausting the stack when
// xml.Unmarshal and the recursive filter walk recurse per level.
const maxFilterDepth = 100

// checkXMLNestingDepth rejects XML whose element nesting exceeds max. It scans
// tokens iteratively so the check itself cannot stack-overflow, and runs before
// the recursive struct unmarshal.
func checkXMLNestingDepth(body []byte, max int) error {
	dec := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return nil // let the real parser report malformed XML
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
			if depth > max {
				return dav.ErrBadRequest("request body XML nesting exceeds %d levels", max)
			}
		case xml.EndElement:
			depth--
		}
	}
}

// Filter trees mirror RFC 4791 §9.7 and sabre's parsed filter arrays.

// CompFilter matches a component by name, sub-filters, or time range.
type CompFilter struct {
	Name         string
	IsNotDefined bool
	TimeRange    *TimeRange
	CompFilters  []CompFilter
	PropFilters  []PropFilter
}

// PropFilter matches a property.
type PropFilter struct {
	Name         string
	IsNotDefined bool
	TimeRange    *TimeRange
	ParamFilters []ParamFilter
	TextMatch    *TextMatch
}

// ParamFilter matches a property parameter.
type ParamFilter struct {
	Name         string
	IsNotDefined bool
	TextMatch    *TextMatch
}

// TextMatch is a collation-aware substring match.
type TextMatch struct {
	Collation string
	Negate    bool
	Value     string
}

// TimeRange bounds a filter; zero times are unbounded.
type TimeRange struct {
	Start, End time.Time
}

// queryXML parses the calendar-query request body.
type queryXML struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:caldav calendar-query"`
	Prop    *struct {
		Names []reqProp `xml:",any"`
	} `xml:"DAV: prop"`
	Filter struct {
		CompFilters []compFilterXML `xml:"comp-filter"`
	} `xml:"filter"`
}

type reqProp struct {
	XMLName xml.Name
	// Expand and ContentType are only meaningful inside calendar-data.
	Expand      *expandXML `xml:"expand"`
	ContentType string     `xml:"content-type,attr"`
}

type expandXML struct {
	Start string `xml:"start,attr"`
	End   string `xml:"end,attr"`
}

type compFilterXML struct {
	Name         string          `xml:"name,attr"`
	IsNotDefined *struct{}       `xml:"is-not-defined"`
	TimeRange    *timeRangeXML   `xml:"time-range"`
	CompFilters  []compFilterXML `xml:"comp-filter"`
	PropFilters  []propFilterXML `xml:"prop-filter"`
}

type propFilterXML struct {
	Name         string           `xml:"name,attr"`
	IsNotDefined *struct{}        `xml:"is-not-defined"`
	TimeRange    *timeRangeXML    `xml:"time-range"`
	ParamFilters []paramFilterXML `xml:"param-filter"`
	TextMatch    *textMatchXML    `xml:"text-match"`
}

type paramFilterXML struct {
	Name         string        `xml:"name,attr"`
	IsNotDefined *struct{}     `xml:"is-not-defined"`
	TextMatch    *textMatchXML `xml:"text-match"`
}

type textMatchXML struct {
	Collation string `xml:"collation,attr"`
	Negate    string `xml:"negate-condition,attr"`
	Value     string `xml:",chardata"`
}

type timeRangeXML struct {
	Start string `xml:"start,attr"`
	End   string `xml:"end,attr"`
}

// CalendarQuery is a parsed calendar-query REPORT.
type CalendarQuery struct {
	// Props are the requested property names.
	Props []xml.Name
	// Expand, when set, expands recurrences in calendar-data.
	Expand *TimeRange
	// JSON requests calendar-data as jCal.
	JSON bool
	// Filter is the root filter (name VCALENDAR).
	Filter CompFilter
}

// prefilterHints returns the top-level component type and time range the backend
// can prefilter candidates on, or "" / zero times when there is no single
// unambiguous constraint. A nested or is-not-defined filter yields no hint, so
// the backend returns a superset and the validator decides.
func (q *CalendarQuery) prefilterHints() (componentType string, start, end time.Time) {
	if len(q.Filter.CompFilters) != 1 {
		return "", time.Time{}, time.Time{}
	}
	inner := q.Filter.CompFilters[0]
	if inner.IsNotDefined {
		return "", time.Time{}, time.Time{}
	}
	componentType = inner.Name
	if inner.TimeRange != nil {
		start, end = inner.TimeRange.Start, inner.TimeRange.End
	}
	return componentType, start, end
}

// ParseCalendarQuery parses a calendar-query REPORT body.
func ParseCalendarQuery(body []byte) (*CalendarQuery, error) {
	if err := checkXMLNestingDepth(body, maxFilterDepth); err != nil {
		return nil, err
	}
	var q queryXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&q); err != nil {
		return nil, err
	}
	if len(q.Filter.CompFilters) != 1 {
		return nil, fmt.Errorf("the {urn:ietf:params:xml:ns:caldav}filter element must have exactly one comp-filter")
	}
	filter, err := convertCompFilter(q.Filter.CompFilters[0])
	if err != nil {
		return nil, err
	}
	out := &CalendarQuery{Filter: filter}
	if q.Prop != nil {
		for _, p := range q.Prop.Names {
			out.Props = append(out.Props, p.XMLName)
			if p.XMLName.Local == "calendar-data" {
				if p.Expand != nil {
					tr, err := parseExpandRange(p.Expand)
					if err != nil {
						return nil, err
					}
					out.Expand = tr
				}
				out.JSON = p.ContentType == "application/calendar+json"
			}
		}
	}
	return out, nil
}

// multigetXML parses the calendar-multiget request body.
type multigetXML struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:caldav calendar-multiget"`
	Prop    *struct {
		Names []reqProp `xml:",any"`
	} `xml:"DAV: prop"`
	Hrefs []string `xml:"DAV: href"`
}

// CalendarMultiget is a parsed calendar-multiget REPORT.
type CalendarMultiget struct {
	Props  []xml.Name
	Expand *TimeRange
	JSON   bool
	Hrefs  []string
}

// ParseCalendarMultiget parses a calendar-multiget REPORT body.
func ParseCalendarMultiget(body []byte) (*CalendarMultiget, error) {
	if err := checkXMLNestingDepth(body, maxFilterDepth); err != nil {
		return nil, err
	}
	var q multigetXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&q); err != nil {
		return nil, err
	}
	out := &CalendarMultiget{Hrefs: q.Hrefs}
	if q.Prop != nil {
		for _, p := range q.Prop.Names {
			out.Props = append(out.Props, p.XMLName)
			if p.XMLName.Local == "calendar-data" {
				if p.Expand != nil {
					tr, err := parseExpandRange(p.Expand)
					if err != nil {
						return nil, err
					}
					out.Expand = tr
				}
				out.JSON = p.ContentType == "application/calendar+json"
			}
		}
	}
	return out, nil
}

func parseExpandRange(e *expandXML) (*TimeRange, error) {
	start, err := parseICalTime(e.Start)
	if err != nil {
		return nil, dav.ErrBadRequest("The 'start' attribute of the expand element is invalid")
	}
	end, err := parseICalTime(e.End)
	if err != nil {
		return nil, dav.ErrBadRequest("The 'end' attribute of the expand element is invalid")
	}
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return nil, dav.ErrBadRequest("The 'start' attribute must come before the 'end' attribute in the expand element")
	}
	return &TimeRange{Start: start, End: end}, nil
}

func parseICalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse("20060102T150405Z", s)
}

func convertCompFilter(in compFilterXML) (CompFilter, error) {
	out := CompFilter{Name: in.Name, IsNotDefined: in.IsNotDefined != nil}
	tr, err := convertTimeRange(in.TimeRange)
	if err != nil {
		return out, err
	}
	out.TimeRange = tr
	for _, cf := range in.CompFilters {
		converted, err := convertCompFilter(cf)
		if err != nil {
			return out, err
		}
		out.CompFilters = append(out.CompFilters, converted)
	}
	for _, pf := range in.PropFilters {
		converted, err := convertPropFilter(pf)
		if err != nil {
			return out, err
		}
		out.PropFilters = append(out.PropFilters, converted)
	}
	return out, nil
}

func convertPropFilter(in propFilterXML) (PropFilter, error) {
	out := PropFilter{Name: in.Name, IsNotDefined: in.IsNotDefined != nil}
	tr, err := convertTimeRange(in.TimeRange)
	if err != nil {
		return out, err
	}
	out.TimeRange = tr
	for _, pf := range in.ParamFilters {
		out.ParamFilters = append(out.ParamFilters, convertParamFilter(pf))
	}
	out.TextMatch = convertTextMatch(in.TextMatch)
	return out, nil
}

func convertParamFilter(in paramFilterXML) ParamFilter {
	return ParamFilter{
		Name:         in.Name,
		IsNotDefined: in.IsNotDefined != nil,
		TextMatch:    convertTextMatch(in.TextMatch),
	}
}

func convertTextMatch(in *textMatchXML) *TextMatch {
	if in == nil {
		return nil
	}
	collation := in.Collation
	if collation == "" {
		collation = "i;ascii-casemap"
	}
	return &TextMatch{
		Collation: collation,
		Negate:    in.Negate == "yes",
		Value:     in.Value,
	}
}

// timeRangeableProps are the date properties sabre allows time-range
// filters on (its validateTimeRange property cases).
var timeRangeableProps = map[string]bool{
	"COMPLETED": true, "CREATED": true, "DTEND": true, "DTSTAMP": true,
	"DTSTART": true, "DUE": true, "LAST-MODIFIED": true,
}

// convertTimeRange rejects malformed time attributes (a bad value must be a
// 400, not a silently unbounded filter).
func convertTimeRange(in *timeRangeXML) (*TimeRange, error) {
	if in == nil {
		return nil, nil
	}
	start, err := parseICalTime(in.Start)
	if err != nil {
		return nil, dav.ErrBadRequest("The 'start' attribute of the time-range element is invalid: %s", in.Start)
	}
	end, err := parseICalTime(in.End)
	if err != nil {
		return nil, dav.ErrBadRequest("The 'end' attribute of the time-range element is invalid: %s", in.End)
	}
	return &TimeRange{Start: start, End: end}, nil
}

// bounds applies sabre's defaults for open-ended ranges.
func (tr *TimeRange) bounds() (time.Time, time.Time) {
	start, end := tr.Start, tr.End
	if start.IsZero() {
		start = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if end.IsZero() {
		end = time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return start, end
}

// Validator checks calendar objects against a filter tree. Port of sabre's
// CalendarQueryValidator. loc resolves floating times.
type Validator struct {
	Loc *time.Location
	// cal is the object being validated (recurring-VALARM checks need
	// access to overridden sibling instances).
	cal *goical.Calendar
}

// Validate reports whether the calendar object matches the root filter.
func (v *Validator) Validate(cal *goical.Calendar, filter CompFilter) (bool, error) {
	if cal.Name != filter.Name {
		return false, nil
	}
	v.cal = cal
	return v.validateComponent(cal.Component, filter.CompFilters, filter.PropFilters)
}

func (v *Validator) loc() *time.Location {
	if v.Loc != nil {
		return v.Loc
	}
	return time.UTC
}

func (v *Validator) validateComponent(parent *goical.Component, compFilters []CompFilter, propFilters []PropFilter) (bool, error) {
	ok, err := v.validateCompFilters(parent, compFilters)
	if err != nil || !ok {
		return false, err
	}
	return v.validatePropFilters(parent, propFilters)
}

func childrenNamed(parent *goical.Component, name string) []*goical.Component {
	var out []*goical.Component
	for _, c := range parent.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func (v *Validator) validateCompFilters(parent *goical.Component, filters []CompFilter) (bool, error) {
nextFilter:
	for _, filter := range filters {
		children := childrenNamed(parent, filter.Name)
		if filter.IsNotDefined {
			if len(children) > 0 {
				return false, nil
			}
			continue
		}
		if len(children) == 0 {
			return false, nil
		}

		if filter.TimeRange != nil {
			start, end := filter.TimeRange.bounds()
			for _, sub := range children {
				in, err := v.componentInTimeRange(sub, parent, start, end)
				if err != nil {
					return false, err
				}
				if in {
					continue nextFilter
				}
			}
			return false, nil
		}

		if len(filter.CompFilters) == 0 && len(filter.PropFilters) == 0 {
			continue
		}
		for _, sub := range children {
			ok, err := v.validateComponent(sub, filter.CompFilters, filter.PropFilters)
			if err != nil {
				return false, err
			}
			if ok {
				continue nextFilter
			}
		}
		return false, nil
	}
	return true, nil
}

func (v *Validator) validatePropFilters(parent *goical.Component, filters []PropFilter) (bool, error) {
nextFilter:
	for _, filter := range filters {
		props := parent.Props.Values(filter.Name)
		if filter.IsNotDefined {
			if len(props) > 0 {
				return false, nil
			}
			continue
		}
		if len(props) == 0 {
			return false, nil
		}

		if filter.TimeRange != nil {
			// Sabre restricts property time-ranges to the date properties
			// and matches with an inclusive end ($start <= dt && $end >= dt).
			if !timeRangeableProps[filter.Name] {
				return false, dav.ErrBadRequest("You cannot create a time-range filter on a %s component", filter.Name)
			}
			start, end := filter.TimeRange.bounds()
			for _, prop := range props {
				t, err := prop.DateTime(v.loc())
				if err != nil {
					continue
				}
				if !start.After(t) && !end.Before(t) {
					continue nextFilter
				}
			}
			return false, nil
		}

		if len(filter.ParamFilters) == 0 && filter.TextMatch == nil {
			continue
		}
		for _, prop := range props {
			ok, err := v.validateParamFilters(&prop, filter.ParamFilters)
			if err != nil {
				return false, err
			}
			if ok && (filter.TextMatch == nil || matchText(prop.Value, filter.TextMatch)) {
				continue nextFilter
			}
		}
		return false, nil
	}
	return true, nil
}

func (v *Validator) validateParamFilters(prop *goical.Prop, filters []ParamFilter) (bool, error) {
nextFilter:
	for _, filter := range filters {
		values := prop.Params.Values(filter.Name)
		if filter.IsNotDefined {
			if len(values) > 0 {
				return false, nil
			}
			continue
		}
		if len(values) == 0 {
			return false, nil
		}
		if filter.TextMatch == nil {
			continue
		}
		for _, value := range values {
			if matchText(value, filter.TextMatch) {
				continue nextFilter
			}
		}
		return false, nil
	}
	return true, nil
}

// matchText ports sabre's StringUtil::textMatch for the two mandatory
// collations (both effectively case-insensitive substring matching; octet is
// case-sensitive).
func matchText(haystack string, tm *TextMatch) bool {
	var matched bool
	switch tm.Collation {
	case "i;octet":
		matched = strings.Contains(haystack, tm.Value)
	default: // i;ascii-casemap, i;unicode-casemap
		matched = strings.Contains(strings.ToLower(haystack), strings.ToLower(tm.Value))
	}
	return tm.Negate != matched
}

// componentInTimeRange dispatches per component type, mirroring sabre's
// validateTimeRange.
func (v *Validator) componentInTimeRange(comp, parent *goical.Component, start, end time.Time) (bool, error) {
	switch comp.Name {
	case goical.CompEvent:
		return ical.EventInTimeRange(comp, start, end, v.loc())
	case "VALARM":
		if parent.Name == goical.CompEvent && v.cal != nil {
			if roption, _ := parent.Props.RecurrenceRule(); roption != nil {
				return ical.AlarmsInTimeRangeRecurring(v.cal, parent, start, end, v.loc())
			}
		}
		return ical.AlarmInTimeRange(comp, parent, start, end, v.loc())
	case goical.CompToDo:
		return ical.TodoInTimeRange(comp, start, end, v.loc())
	case "VJOURNAL":
		return ical.JournalInTimeRange(comp, start, end, v.loc())
	default:
		return false, dav.ErrBadRequest("time-range filters are not supported on %s components", comp.Name)
	}
}
