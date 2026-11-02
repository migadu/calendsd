package caldav

import (
	"context"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/migadu/calendsd/internal/acl"
	"github.com/migadu/calendsd/internal/dav"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
	"github.com/migadu/calendsd/internal/pathutil"
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
	// AddVTimezones attaches a generated VTIMEZONE to a PUT that uses an IANA
	// TZID without defining it (RFC 5545 §3.6.5; docs/dst-recurring-events.md
	// §5). Off by default, matching sabre, which stores such bodies as sent;
	// the calendsd composition turns it on.
	AddVTimezones bool
	// MaxAttendees caps the distinct ATTENDEEs an object may carry on PUT
	// (403 max-attendees-per-instance); zero means DefaultMaxAttendees. The
	// scheduling plugin applies the same cap as a backstop (audit 4.4).
	MaxAttendees int

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

// Features matches sabre's CalDAV\Plugin::getFeatures (calendar-proxy is the
// calendarserver delegation extension, RFC-less but advertised by sabre).
func (p *Plugin) Features() []string { return []string{"calendar-access", "calendar-proxy"} }

func (p *Plugin) Methods(ctx context.Context, path string) []string { return []string{"MKCALENDAR"} }

func (p *Plugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	switch r.Method {
	case "MKCALENDAR":
		return true, p.mkcalendar(w, r)
	case "GET":
		return p.handleExport(w, r)
	case "POST":
		return p.handleAddMember(w, r)
	}
	return false, nil
}

// PropFind implements dav.PropFindHook with the CalDAV properties.
func (p *Plugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	cal := func(local string) xml.Name { return xml.Name{Space: davxml.NSCalDAV, Local: local} }

	if ph, isPrincipal := node.(acl.PrincipalHolder); isPrincipal {
		pf.Handle(cal("calendar-home-set"), func() any {
			_, name := pathutil.Split(ph.PrincipalURL())
			return davxml.Href{p.server.Href("calendars/"+name) + "/"}
		})
	}

	if _, isCal := node.(*Calendar); isCal {
		// RFC 5995 Add-Member: POSTing a resource here lets the server
		// assign its name. The URL is the collection plus ";add-member".
		pf.Handle(xml.Name{Space: davxml.NSDav, Local: "add-member"}, func() any {
			return davxml.Href{p.server.Href(pf.Path()) + "/" + addMemberSegment}
		})
		// Constant per-calendar properties sabre emits on every calendar.
		pf.Handle(cal("max-resource-size"), func() any { return MaxResourceSize })
		pf.Handle(cal("max-attendees-per-instance"), func() any {
			return strconv.Itoa(effectiveMaxAttendees(p.MaxAttendees))
		})
		pf.Handle(cal("supported-calendar-data"), func() any {
			return davxml.Raw(`<cal:calendar-data content-type="text/calendar" version="2.0"/>` +
				`<cal:calendar-data content-type="application/calendar+json"/>`)
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
