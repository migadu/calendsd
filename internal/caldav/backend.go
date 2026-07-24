// Package caldav implements the CalDAV layer: calendar nodes, the CalDAV
// plugin (properties + reports), the calendar-query engine, and the storage
// backend abstraction. Port of sabre's lib/CalDAV.
package caldav

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"sort"
	"time"

	"calserver/internal/dav"
)

// CalendarID addresses a calendar by the sabre backend convention: the
// shared calendars row plus the per-principal calendarinstances row
// (PLAN.md §5). In-memory backends leave Instance empty.
type CalendarID struct {
	Calendar string
	Instance string
}

// CalendarInfo describes one calendar of a principal. Props carries
// clark-named calendar properties (displayname, supported-calendar-
// component-set, calendar-timezone, ...), mirroring the array rows sabre's
// backends return.
type CalendarInfo struct {
	// ID is the backend's calendar identifier.
	ID CalendarID
	// URI is the calendar's path segment inside the calendar home.
	URI string
	// PrincipalURI owns the calendar.
	PrincipalURI string
	// Props holds calendar properties in clark-name keys.
	Props map[xml.Name]any
	// ShareAccess is the sharing access level of this calendar view
	// (AccessNotShared/AccessSharedOwner for the owner's own view).
	ShareAccess int
}

// ObjectInfo is one calendar object (event/todo). Port of the row arrays
// sabre backends return.
type ObjectInfo struct {
	URI          string
	CalendarID   CalendarID
	CalendarData []byte
	// ETag, if empty, is derived from CalendarData (md5, quoted).
	ETag string
	Size int64
	// Component is the object's component type (VEVENT/VTODO), used for
	// the content-type hint clients like DAVx5 read.
	Component string
}

// QuotedETag returns the object's ETag surrounded by quotes.
func (o *ObjectInfo) QuotedETag() string {
	if o.ETag != "" {
		return o.ETag
	}
	sum := md5.Sum(o.CalendarData)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// Backend provides calendar storage. Port of the sabre
// CalDAV\Backend\BackendInterface subset the MVP needs.
type Backend interface {
	// CalendarsForUser lists the calendars owned by a principal.
	CalendarsForUser(principalURI string) ([]CalendarInfo, error)
	// CreateCalendar makes a new calendar and returns its ID.
	CreateCalendar(principalURI, calendarURI string, props map[xml.Name]any) (CalendarID, error)
	// DeleteCalendar removes a calendar with its objects.
	DeleteCalendar(calendarID CalendarID) error
	// UpdateCalendar registers PropPatch handlers for the calendar
	// properties the backend persists (sabre's updateCalendar).
	UpdateCalendar(calendarID CalendarID, pp *dav.PropPatch)

	// CalendarObjects lists all objects of a calendar.
	CalendarObjects(calendarID CalendarID) ([]ObjectInfo, error)
	// CalendarQuery returns the URIs of objects that could match a query with
	// the given top-level component type ("" = no constraint) and optional
	// time range (zero Start/End = unconstrained). It is a coarse prefilter
	// that must return a superset of the true matches; the caller validates
	// each candidate. Port of sabre's BackendInterface::calendarQuery.
	CalendarQuery(calendarID CalendarID, componentType string, start, end time.Time) ([]string, error)
	// CalendarObject fetches one object, or nil if absent.
	CalendarObject(calendarID CalendarID, objectURI string) (*ObjectInfo, error)
	// CreateCalendarObject stores a new object, returning its quoted etag.
	CreateCalendarObject(calendarID CalendarID, objectURI string, data []byte) (string, error)
	// UpdateCalendarObject replaces an object, returning its quoted etag.
	UpdateCalendarObject(calendarID CalendarID, objectURI string, data []byte) (string, error)
	// DeleteCalendarObject removes an object.
	DeleteCalendarObject(calendarID CalendarID, objectURI string) error

	// ChangesForCalendar returns changes since syncToken ("" = initial
	// sync); nil means sync is unsupported or the token is unknown.
	ChangesForCalendar(calendarID CalendarID, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error)
}

// MockBackend is the in-memory backend used by tests and the corpus runner.
// Port of sabre's CalDAV\Backend\Mock.
type MockBackend struct {
	Calendars []CalendarInfo
	// Objects maps calendar ID → object URI → object.
	Objects map[CalendarID]map[string]*ObjectInfo

	nextID int
}

func (m *MockBackend) CalendarsForUser(principalURI string) ([]CalendarInfo, error) {
	var out []CalendarInfo
	for _, c := range m.Calendars {
		if c.PrincipalURI == principalURI {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *MockBackend) CreateCalendar(principalURI, calendarURI string, props map[xml.Name]any) (CalendarID, error) {
	m.nextID++
	id := CalendarID{Calendar: "mock-" + hex.EncodeToString([]byte{byte(m.nextID)})}
	info := CalendarInfo{ID: id, URI: calendarURI, PrincipalURI: principalURI, Props: map[xml.Name]any{}}
	for k, v := range props {
		info.Props[k] = v
	}
	m.Calendars = append(m.Calendars, info)
	return id, nil
}

func (m *MockBackend) DeleteCalendar(calendarID CalendarID) error {
	for i, c := range m.Calendars {
		if c.ID == calendarID {
			m.Calendars = append(m.Calendars[:i], m.Calendars[i+1:]...)
			break
		}
	}
	delete(m.Objects, calendarID)
	return nil
}

func (m *MockBackend) CalendarObjects(calendarID CalendarID) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for _, o := range m.Objects[calendarID] {
		out = append(out, *o)
	}
	return out, nil
}

// CalendarQuery returns every object URI (a valid superset); the mock leaves
// all filtering to the caller's validator.
func (m *MockBackend) CalendarQuery(calendarID CalendarID, componentType string, start, end time.Time) ([]string, error) {
	var out []string
	for uri := range m.Objects[calendarID] {
		out = append(out, uri)
	}
	sort.Strings(out)
	return out, nil
}

func (m *MockBackend) CalendarObject(calendarID CalendarID, objectURI string) (*ObjectInfo, error) {
	o, exists := m.Objects[calendarID][objectURI]
	if !exists {
		return nil, nil
	}
	copied := *o
	return &copied, nil
}

func (m *MockBackend) CreateCalendarObject(calendarID CalendarID, objectURI string, data []byte) (string, error) {
	if m.Objects == nil {
		m.Objects = map[CalendarID]map[string]*ObjectInfo{}
	}
	if m.Objects[calendarID] == nil {
		m.Objects[calendarID] = map[string]*ObjectInfo{}
	}
	o := &ObjectInfo{URI: objectURI, CalendarID: calendarID, CalendarData: data}
	m.Objects[calendarID][objectURI] = o
	return o.QuotedETag(), nil
}

func (m *MockBackend) UpdateCalendarObject(calendarID CalendarID, objectURI string, data []byte) (string, error) {
	return m.CreateCalendarObject(calendarID, objectURI, data)
}

func (m *MockBackend) DeleteCalendarObject(calendarID CalendarID, objectURI string) error {
	delete(m.Objects[calendarID], objectURI)
	return nil
}

// UpdateCalendar accepts all mutations into the in-memory props, like
// sabre's mock.
func (m *MockBackend) UpdateCalendar(calendarID CalendarID, pp *dav.PropPatch) {
	pp.HandleRemaining(func(values map[xml.Name]any) (map[xml.Name]int, bool) {
		for i := range m.Calendars {
			if m.Calendars[i].ID != calendarID {
				continue
			}
			for n, v := range values {
				if v == nil {
					delete(m.Calendars[i].Props, n)
				} else {
					m.Calendars[i].Props[n] = v
				}
			}
		}
		return nil, true
	})
}

// ChangesForCalendar reports no sync support, like sabre's plain mock.
func (m *MockBackend) ChangesForCalendar(CalendarID, string, int, int) (*dav.ChangeSet, error) {
	return nil, nil
}
