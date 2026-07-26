package caldav

import (
	"encoding/xml"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"

	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
)

// handleExport implements sabre's ICSExportPlugin: GET ?export on a calendar
// streams the merged calendar as iCalendar or jCal, with optional
// start/end/expand/componentType filtering.
func (p *Plugin) handleExport(w http.ResponseWriter, r *http.Request) (bool, error) {
	if !r.URL.Query().Has("export") {
		return false, nil
	}
	path, err := p.server.Path(r)
	if err != nil {
		return true, err
	}
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return true, err
	}
	calNode, isCal := node.(*Calendar)
	if !isCal {
		return false, nil // not a calendar; let other handlers try
	}

	query := r.URL.Query()
	var start, end time.Time
	if v := query.Get("start"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return true, dav.ErrBadRequest("The start= parameter must contain a unix timestamp")
		}
		start = time.Unix(n, 0).UTC()
	}
	if v := query.Get("end"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return true, dav.ErrBadRequest("The end= parameter must contain a unix timestamp")
		}
		end = time.Unix(n, 0).UTC()
	}
	expand := false
	componentType := ""
	if v := query.Get("expand"); v != "" && v != "0" {
		if start.IsZero() || end.IsZero() {
			return true, dav.ErrBadRequest("If you'd like to expand recurrences, you must specify both a start= and end= parameter.")
		}
		expand = true
		componentType = "VEVENT"
	}
	if v := query.Get("componentType"); v != "" {
		if v != "VEVENT" && v != "VTODO" && v != "VJOURNAL" {
			return true, dav.ErrBadRequest("You are not allowed to search for components of type: %s here", v)
		}
		componentType = v
	}

	format := negotiateExportFormat(r.Header.Get("Accept"))
	if a := query.Get("accept"); a == "application/calendar+json" || a == "jcal" {
		format = "application/calendar+json"
	}

	blobs, err := p.exportBlobs(calNode, path, start, end, componentType)
	if err != nil {
		return true, err
	}
	merged, err := mergeCalendars(calNode.Info, blobs)
	if err != nil {
		return true, err
	}
	if expand {
		merged, err = ical.Expand(merged, start, end, p.calendarTimeZone(path))
		if err != nil {
			return true, err
		}
	}

	var body []byte
	ext := ".ics"
	if format == "application/calendar+json" {
		ext = ".json"
		body, err = ical.JCal(merged)
		if err != nil {
			return true, err
		}
	} else {
		body = ical.EncodeLenient(merged)
	}

	filename := exportFilenameRe.ReplaceAllString(calNode.Name(), "") +
		"-" + time.Now().Format("2006-01-02") + ext
	h := w.Header()
	h.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	h.Set("Content-Type", format)
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	return true, err
}

var exportFilenameRe = regexp.MustCompile(`[^a-zA-Z0-9-_ ]`)

// negotiateExportFormat picks between text/calendar and
// application/calendar+json from the Accept header; anything else defaults
// to text/calendar, like sabre.
func negotiateExportFormat(accept string) string {
	for _, part := range strings.Split(accept, ",") {
		mediaType, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		switch strings.TrimSpace(mediaType) {
		case "text/calendar":
			return "text/calendar"
		case "application/calendar+json":
			return "application/calendar+json"
		}
	}
	return "text/calendar"
}

// maxExportObjects caps how many objects a single export merges. A whole-
// calendar export loads, decodes, merges, (optionally) recurrence-expands and
// re-encodes every object in memory; without a bound a very large calendar is a
// memory/CPU DoS on an unauthenticated-size GET. Beyond this, the client must
// narrow with start=/end=.
const maxExportObjects = 50000

// exportBlobs collects the calendar objects to merge, filtered when
// start/end/componentType are given.
func (p *Plugin) exportBlobs(calNode *Calendar, path string, start, end time.Time, componentType string) ([][]byte, error) {
	objects, err := calNode.backend.CalendarObjects(calNode.Info.ID)
	if err != nil {
		return nil, err
	}
	exportAll := start.IsZero() && end.IsZero() && componentType == ""
	var out [][]byte
	for _, obj := range objects {
		if len(out) >= maxExportObjects {
			return nil, dav.ErrForbidden(
				"This calendar has too many objects to export at once (limit %d); narrow the export with start= and end= parameters.",
				maxExportObjects)
		}
		if !exportAll {
			name := componentType
			if name == "" {
				name = "VEVENT"
			}
			filter := CompFilter{Name: "VCALENDAR", CompFilters: []CompFilter{{
				Name:      name,
				TimeRange: &TimeRange{Start: start, End: end},
			}}}
			cal, err := ical.Decode(obj.CalendarData)
			if err != nil {
				continue
			}
			v := &Validator{Loc: p.calendarTimeZone(path)}
			match, err := v.Validate(cal, filter)
			if err != nil || !match {
				continue
			}
		}
		out = append(out, obj.CalendarData)
	}
	return out, nil
}

// mergeCalendars builds one VCALENDAR from many objects: calendar-level
// X-WR-CALNAME/X-APPLE-CALENDAR-COLOR, VTIMEZONEs deduplicated by TZID, then
// every VEVENT/VTODO/VJOURNAL. Port of ICSExportPlugin::mergeObjects.
func mergeCalendars(info CalendarInfo, blobs [][]byte) (*goical.Calendar, error) {
	merged := goical.NewCalendar()
	setPlain(merged.Props, goical.PropVersion, "2.0")
	setPlain(merged.Props, goical.PropProductID, "-//calserver//calserver//EN")
	setPlain(merged.Props, "CALSCALE", "GREGORIAN")
	if v, ok := info.Props[xmlNameDav("displayname")].(string); ok {
		setPlain(merged.Props, "X-WR-CALNAME", v)
	}
	if v, ok := info.Props[xmlNameApple("calendar-color")].(string); ok {
		setPlain(merged.Props, "X-APPLE-CALENDAR-COLOR", v)
	}

	seenTZ := map[string]bool{}
	var timezones, objects []*goical.Component
	for _, blob := range blobs {
		cal, err := ical.Decode(blob)
		if err != nil {
			return nil, dav.ErrBadRequest("could not parse calendar object: %s", err)
		}
		for _, child := range cal.Children {
			switch child.Name {
			case goical.CompEvent, goical.CompToDo, "VJOURNAL":
				objects = append(objects, child)
			case goical.CompTimezone:
				tzid := ""
				if p := child.Props.Get("TZID"); p != nil {
					tzid = p.Value
				}
				if seenTZ[tzid] {
					continue
				}
				seenTZ[tzid] = true
				timezones = append(timezones, child)
			}
		}
	}
	merged.Children = append(merged.Children, timezones...)
	merged.Children = append(merged.Children, objects...)
	return merged, nil
}

// setPlain writes a property without forcing a VALUE parameter (SetText
// would tag X- properties with VALUE=TEXT, which sabre does not emit).
func setPlain(props goical.Props, name, value string) {
	prop := goical.NewProp(name)
	prop.Value = value
	props.Set(prop)
}

func xmlNameDav(local string) xml.Name {
	return xml.Name{Space: "DAV:", Local: local}
}

func xmlNameApple(local string) xml.Name {
	return xml.Name{Space: "http://apple.com/ns/ical/", Local: local}
}
