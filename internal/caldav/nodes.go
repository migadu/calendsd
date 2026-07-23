package caldav

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/dav"
)

// CalendarRoot is the /calendars collection: one CalendarHome per principal.
// Port of sabre's CalDAV\CalendarRoot.
type CalendarRoot struct {
	name       string
	principals acl.Backend
	backend    Backend
	// principalPrefix scopes which principals get homes.
	principalPrefix string
}

// NewCalendarRoot builds the calendar root (conventionally named
// "calendars") over the principal and calendar backends.
func NewCalendarRoot(principals acl.Backend, backend Backend, name string) *CalendarRoot {
	return &CalendarRoot{name: name, principals: principals, backend: backend, principalPrefix: "principals"}
}

func (r *CalendarRoot) Name() string { return r.name }
func (r *CalendarRoot) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (r *CalendarRoot) Delete() error           { return dav.ErrForbidden("Permission denied to delete node") }
func (r *CalendarRoot) LastModified() time.Time { return time.Time{} }

func (r *CalendarRoot) Children() ([]dav.Node, error) {
	principals, err := r.principals.PrincipalsByPrefix(r.principalPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]dav.Node, len(principals))
	for i, p := range principals {
		out[i] = NewCalendarHome(r.backend, p.URI)
	}
	return out, nil
}

func (r *CalendarRoot) Child(name string) (dav.Node, error) {
	p, err := r.principals.PrincipalByPath(r.principalPrefix + "/" + name)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, dav.ErrNotFound("Principal with name %s not found", name)
	}
	return NewCalendarHome(r.backend, p.URI), nil
}

func (r *CalendarRoot) ChildExists(name string) bool {
	p, err := r.principals.PrincipalByPath(r.principalPrefix + "/" + name)
	return err == nil && p != nil
}

func (r *CalendarRoot) CreateFile(string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in this collection is not supported")
}

func (r *CalendarRoot) CreateDirectory(string) error {
	return dav.ErrMethodNotAllowed("Creating collections in this collection is not supported")
}

// CalendarHome holds one principal's calendars. Port of sabre's
// CalDAV\CalendarHome.
type CalendarHome struct {
	backend      Backend
	principalURI string
}

// NewCalendarHome builds a calendar home for a principal.
func NewCalendarHome(backend Backend, principalURI string) *CalendarHome {
	return &CalendarHome{backend: backend, principalURI: principalURI}
}

func (h *CalendarHome) Name() string {
	i := strings.LastIndexByte(h.principalURI, '/')
	return h.principalURI[i+1:]
}

func (h *CalendarHome) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (h *CalendarHome) Delete() error           { return dav.ErrForbidden("Permission denied to delete node") }
func (h *CalendarHome) LastModified() time.Time { return time.Time{} }

func (h *CalendarHome) Children() ([]dav.Node, error) {
	infos, err := h.backend.CalendarsForUser(h.principalURI)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, info := range infos {
		out = append(out, NewCalendar(h.backend, info))
	}
	// Scheduling-capable backends expose the inbox and outbox, like
	// sabre's CalendarHome.
	if sb, ok := h.backend.(SchedulingBackend); ok {
		out = append(out, NewInbox(sb, h.principalURI), NewOutbox(h.principalURI))
	}
	return out, nil
}

func (h *CalendarHome) Child(name string) (dav.Node, error) {
	if sb, ok := h.backend.(SchedulingBackend); ok {
		switch name {
		case "inbox":
			return NewInbox(sb, h.principalURI), nil
		case "outbox":
			return NewOutbox(h.principalURI), nil
		}
	}
	infos, err := h.backend.CalendarsForUser(h.principalURI)
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if info.URI == name {
			return NewCalendar(h.backend, info), nil
		}
	}
	return nil, dav.ErrNotFound("Node with name '%s' could not be found", name)
}

func (h *CalendarHome) ChildExists(name string) bool {
	_, err := h.Child(name)
	return err == nil
}

func (h *CalendarHome) CreateFile(string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating new files in this collection is not supported")
}

// CreateDirectory refuses plain MKCOL; calendars come via MKCALENDAR (the
// plugin calls CreateCalendar).
func (h *CalendarHome) CreateDirectory(string) error {
	return dav.ErrMethodNotAllowed("Creating new collections in this collection is not supported")
}

// CreateCalendar makes a new calendar in this home (extended MKCOL and
// MKCALENDAR land here).
func (h *CalendarHome) CreateCalendar(name string, props map[xml.Name]any) error {
	_, err := h.backend.CreateCalendar(h.principalURI, name, props)
	return err
}

// Owner returns the owning principal.
func (h *CalendarHome) Owner() string { return h.principalURI }

// ACL grants the owning principal full access, like sabre's ACLTrait
// default on CalendarHome.
func (h *CalendarHome) ACL() []acl.ACE {
	return []acl.ACE{{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true}}
}

// Calendar is one calendar collection. Port of sabre's CalDAV\Calendar.
type Calendar struct {
	backend Backend
	Info    CalendarInfo
}

// NewCalendar wraps calendar info into a node.
func NewCalendar(backend Backend, info CalendarInfo) *Calendar {
	return &Calendar{backend: backend, Info: info}
}

func (c *Calendar) Name() string { return c.Info.URI }
func (c *Calendar) SetName(string) error {
	return dav.ErrMethodNotAllowed("Renaming calendars is not yet supported")
}
func (c *Calendar) Delete() error           { return c.backend.DeleteCalendar(c.Info.ID) }
func (c *Calendar) LastModified() time.Time { return time.Time{} }

func (c *Calendar) Children() ([]dav.Node, error) {
	objs, err := c.backend.CalendarObjects(c.Info.ID)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, obj := range objs {
		obj := obj
		out = append(out, NewCalendarObject(c.backend, c.Info, obj))
	}
	return out, nil
}

func (c *Calendar) Child(name string) (dav.Node, error) {
	obj, err := c.backend.CalendarObject(c.Info.ID, name)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, dav.ErrNotFound("Calendar object not found")
	}
	return NewCalendarObject(c.backend, c.Info, *obj), nil
}

func (c *Calendar) ChildExists(name string) bool {
	obj, err := c.backend.CalendarObject(c.Info.ID, name)
	return err == nil && obj != nil
}

func (c *Calendar) CreateFile(name string, data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	return c.backend.CreateCalendarObject(c.Info.ID, name, raw)
}

func (c *Calendar) CreateDirectory(string) error {
	return dav.ErrMethodNotAllowed("Creating collections in calendar objects is not allowed")
}

// Props implements dav.PropNode with the calendar's stored properties.
func (c *Calendar) Props(names []xml.Name) (map[xml.Name]any, error) {
	if len(names) == 0 {
		return c.Info.Props, nil
	}
	out := map[xml.Name]any{}
	for _, n := range names {
		if v, exists := c.Info.Props[n]; exists {
			out[n] = v
		}
	}
	return out, nil
}

func (c *Calendar) PropPatch(pp *dav.PropPatch) {
	c.backend.UpdateCalendar(c.Info.ID, pp)
}

// Owner returns the owning principal.
func (c *Calendar) Owner() string { return c.Info.PrincipalURI }

// SyncToken implements dav.SyncCollection from the backend-provided
// {http://sabredav.org/ns}sync-token property.
func (c *Calendar) SyncToken() string {
	if v, ok := c.Info.Props[xml.Name{Space: "http://sabredav.org/ns", Local: "sync-token"}].(string); ok {
		return v
	}
	return ""
}

// Changes implements dav.SyncCollection.
func (c *Calendar) Changes(syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	return c.backend.ChangesForCalendar(c.Info.ID, syncToken, syncLevel, limit)
}

// SupportedPrivileges extends the default tree with read-free-busy, like
// sabre Calendar::getSupportedPrivilegeSet.
func (c *Calendar) SupportedPrivileges() *acl.Privilege {
	return &acl.Privilege{Name: "{DAV:}all", Abstract: true, Aggregates: []acl.Privilege{
		{Name: "{DAV:}read", Aggregates: []acl.Privilege{
			{Name: "{DAV:}read-acl"},
			{Name: "{DAV:}read-current-user-privilege-set"},
			{Name: "{urn:ietf:params:xml:ns:caldav}read-free-busy"},
		}},
		{Name: "{DAV:}write", Aggregates: []acl.Privilege{
			{Name: "{DAV:}write-properties"},
			{Name: "{DAV:}write-content"},
			{Name: "{DAV:}unlock"},
			{Name: "{DAV:}bind"},
			{Name: "{DAV:}unbind"},
			{Name: "{DAV:}write-acl"},
		}},
	}}
}

// readOnly reports sabre's {http://sabredav.org/ns}read-only marker, set on
// calendars shared with ACCESS_READ.
func (c *Calendar) readOnly() bool {
	v, ok := c.Info.Props[xml.Name{Space: "http://sabredav.org/ns", Local: "read-only"}]
	if !ok {
		return false
	}
	s, isString := v.(string)
	return !isString || (s != "" && s != "0")
}

// ACL mirrors sabre Calendar::getACL: owner + proxies read, authenticated
// free-busy, and write only when the view is not read-only.
func (c *Calendar) ACL() []acl.ACE {
	owner := c.Owner()
	aces := []acl.ACE{
		{Privilege: "{DAV:}read", Principal: owner, Protected: true},
		{Privilege: "{DAV:}read", Principal: owner + "/calendar-proxy-write", Protected: true},
		{Privilege: "{DAV:}read", Principal: owner + "/calendar-proxy-read", Protected: true},
		{Privilege: "{urn:ietf:params:xml:ns:caldav}read-free-busy", Principal: "{DAV:}authenticated", Protected: true},
	}
	if !c.readOnly() {
		aces = append(aces,
			acl.ACE{Privilege: "{DAV:}write", Principal: owner, Protected: true},
			acl.ACE{Privilege: "{DAV:}write", Principal: owner + "/calendar-proxy-write", Protected: true})
	}
	return aces
}

// CalendarObject is a single stored event/todo. Port of sabre's
// CalDAV\CalendarObject.
type CalendarObject struct {
	backend  Backend
	calendar CalendarInfo
	obj      ObjectInfo
}

// NewCalendarObject wraps object info into a file node.
func NewCalendarObject(backend Backend, calendar CalendarInfo, obj ObjectInfo) *CalendarObject {
	return &CalendarObject{backend: backend, calendar: calendar, obj: obj}
}

func (o *CalendarObject) Name() string { return o.obj.URI }
func (o *CalendarObject) SetName(string) error {
	return dav.ErrMethodNotAllowed("Renaming calendar objects is not yet supported")
}
func (o *CalendarObject) Delete() error {
	return o.backend.DeleteCalendarObject(o.calendar.ID, o.obj.URI)
}
func (o *CalendarObject) LastModified() time.Time { return time.Time{} }

func (o *CalendarObject) Get() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(o.obj.CalendarData)), nil
}

func (o *CalendarObject) Put(data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	etag, err := o.backend.UpdateCalendarObject(o.calendar.ID, o.obj.URI, raw)
	if err != nil {
		return "", err
	}
	o.obj.CalendarData = raw
	return etag, nil
}

// ContentType includes the component hint, like sabre's CalendarObject.
func (o *CalendarObject) ContentType() string {
	ct := "text/calendar; charset=utf-8"
	if o.obj.Component != "" {
		ct += "; component=" + strings.ToLower(o.obj.Component)
	}
	return ct
}
func (o *CalendarObject) Size() int64  { return int64(len(o.obj.CalendarData)) }
func (o *CalendarObject) ETag() string { return o.obj.QuotedETag() }

// Data exposes the raw iCalendar payload.
func (o *CalendarObject) Data() []byte { return o.obj.CalendarData }

// Owner returns the owning principal.
func (o *CalendarObject) Owner() string { return o.calendar.PrincipalURI }

// ACL mirrors sabre Calendar::getChildACL: the calendar's owner (and its
// proxies) read the objects; write only on non-read-only views.
func (o *CalendarObject) ACL() []acl.ACE {
	owner := o.Owner()
	aces := []acl.ACE{
		{Privilege: "{DAV:}read", Principal: owner, Protected: true},
		{Privilege: "{DAV:}read", Principal: owner + "/calendar-proxy-write", Protected: true},
		{Privilege: "{DAV:}read", Principal: owner + "/calendar-proxy-read", Protected: true},
	}
	if !(&Calendar{Info: o.calendar}).readOnly() {
		aces = append(aces,
			acl.ACE{Privilege: "{DAV:}write", Principal: owner, Protected: true},
			acl.ACE{Privilege: "{DAV:}write", Principal: owner + "/calendar-proxy-write", Protected: true})
	}
	return aces
}
