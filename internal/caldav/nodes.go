package caldav

import (
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/caldav/ical"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// CalendarRoot is the /calendars collection: one CalendarHome per principal.
// Port of sabre's CalDAV\CalendarRoot.
type CalendarRoot struct {
	name       string
	principals acl.Backend
	backend    Backend
	// principalPrefix scopes which principals get homes.
	principalPrefix string
	// AutoProvision recreates the default "calendar" when a principal has
	// none left (Calendar-Server behavior); off by default (sabre parity).
	AutoProvision bool
	// PerUserProps marks Calendar-Server per-user-properties mode (see
	// Calendar.PerUserProps); off by default.
	PerUserProps bool
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
		home := NewCalendarHome(r.backend, p.URI)
		home.autoProvision = r.AutoProvision
		home.perUserProps = r.PerUserProps
		out[i] = home
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
	home := NewCalendarHome(r.backend, p.URI)
	home.autoProvision = r.AutoProvision
	home.perUserProps = r.PerUserProps
	return home, nil
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
	backend       Backend
	principalURI  string
	autoProvision bool
	perUserProps  bool
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
	infos, err := h.provisioned()
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, info := range infos {
		cal := NewCalendar(h.backend, info)
		cal.PerUserProps = h.perUserProps
		out = append(out, cal)
	}
	// Scheduling-capable backends expose the inbox and outbox, like
	// sabre's CalendarHome.
	if sb, ok := h.backend.(SchedulingBackend); ok {
		out = append(out, NewInbox(sb, h.principalURI), NewOutbox(h.principalURI))
	}
	if nb, ok := h.backend.(NotificationsBackend); ok {
		out = append(out, NewNotifications(nb, h.principalURI))
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
	if nb, ok := h.backend.(NotificationsBackend); ok && name == "notifications" {
		return NewNotifications(nb, h.principalURI), nil
	}
	infos, err := h.provisioned()
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if info.URI == name {
			cal := NewCalendar(h.backend, info)
			cal.PerUserProps = h.perUserProps
			return cal, nil
		}
	}
	return nil, dav.ErrNotFound("Node with name '%s' could not be found", name)
}

// provisioned lists the principal's calendars, recreating the default
// "calendar" first when auto-provisioning is on and none are left (Calendar
// Server regenerates it after deletion; the CalDAVTester suite relies on it).
func (h *CalendarHome) provisioned() ([]CalendarInfo, error) {
	infos, err := h.backend.CalendarsForUser(h.principalURI)
	if err != nil || !h.autoProvision || len(infos) > 0 {
		return infos, err
	}
	if _, err := h.backend.CreateCalendar(h.principalURI, "calendar", map[xml.Name]any{
		{Space: davxml.NSDav, Local: "displayname"}: "calendar",
	}); err != nil {
		return nil, err
	}
	return h.backend.CalendarsForUser(h.principalURI)
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

// CreateExtendedCollection implements dav.ExtendedCollection: calendars and
// subscriptions, depending on the resource types. Port of sabre's
// CalendarHome::createExtendedCollection.
func (h *CalendarHome) CreateExtendedCollection(name string, resourceTypes []xml.Name, props map[xml.Name]any) error {
	isSubscription := false
	isCalendar := false
	for _, rt := range resourceTypes {
		if rt == nameSubscribed {
			isSubscription = true
		}
		if rt == nameCalendar {
			isCalendar = true
		}
	}
	switch {
	case isSubscription:
		subs, supported := h.backend.(SubscriptionsBackend)
		if !supported {
			return dav.ErrForbidden("This backend does not support subscriptions")
		}
		_, err := subs.CreateSubscription(h.principalURI, name, props)
		return err
	case isCalendar:
		return h.CreateCalendar(name, props)
	default:
		return dav.ErrForbidden("Unable to create a collection of this type in the calendar home")
	}
}

// Owner returns the owning principal.
func (h *CalendarHome) Owner() string { return h.principalURI }

// ACL grants the owning principal full access, like sabre's ACLTrait
// default on CalendarHome.
// ACL mirrors sabre CalendarHome::getACL: the owner plus their calendar-proxy
// groups (write proxies read+write, read proxies read).
func (h *CalendarHome) ACL() []acl.ACE {
	return []acl.ACE{
		{Privilege: "{DAV:}read", Principal: h.principalURI, Protected: true},
		{Privilege: "{DAV:}write", Principal: h.principalURI, Protected: true},
		{Privilege: "{DAV:}read", Principal: h.principalURI + "/calendar-proxy-write", Protected: true},
		{Privilege: "{DAV:}write", Principal: h.principalURI + "/calendar-proxy-write", Protected: true},
		{Privilege: "{DAV:}read", Principal: h.principalURI + "/calendar-proxy-read", Protected: true},
	}
}

// Calendar is one calendar collection. Port of sabre's CalDAV\Calendar.
type Calendar struct {
	backend Backend
	Info    CalendarInfo
	// PerUserProps marks Calendar-Server per-user-properties mode: read
	// delegates additionally hold {DAV:}write-properties (their property
	// writes are per-user, so they cannot damage shared state).
	PerUserProps bool
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

// QueryObjects returns the objects passing the backend's coarse prefilter for a
// calendar-query with the given top-level component type and time range. The
// caller applies the full validator to each; this only narrows what is loaded.
func (c *Calendar) QueryObjects(componentType string, start, end time.Time) ([]*CalendarObject, error) {
	uris, err := c.backend.CalendarQuery(c.Info.ID, componentType, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]*CalendarObject, 0, len(uris))
	for _, uri := range uris {
		obj, err := c.backend.CalendarObject(c.Info.ID, uri)
		if err != nil {
			return nil, err
		}
		if obj != nil {
			out = append(out, NewCalendarObject(c.backend, c.Info, *obj))
		}
	}
	return out, nil
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

var supportedComponentRe = regexp.MustCompile(`name="([A-Za-z]+)"`)

// SupportedComponents lists the component types this calendar accepts, parsed
// from supported-calendar-component-set. Absent the property it defaults to all
// three, matching sabre's validateICalendar.
func (c *Calendar) SupportedComponents() []string {
	name := xml.Name{Space: davxml.NSCalDAV, Local: "supported-calendar-component-set"}
	if raw, ok := c.Info.Props[name].(davxml.Raw); ok {
		var out []string
		for _, m := range supportedComponentRe.FindAllStringSubmatch(string(raw), -1) {
			out = append(out, strings.ToUpper(m[1]))
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"VEVENT", "VTODO", "VJOURNAL"}
}

// Props implements dav.PropNode with the calendar's stored properties.
func (c *Calendar) Props(names []xml.Name) (map[xml.Name]any, error) {
	if len(names) == 0 {
		return c.Info.Props, nil
	}
	out := map[xml.Name]any{}
	for _, n := range names {
		// {cs}invite is computed from the live sharee list rather than stored,
		// so clients (the owner's UI) can read who a calendar is shared with.
		// It is served on request only — never in the allprops bulk above,
		// which would add a query per calendar to every home listing.
		if n == (xml.Name{Space: davxml.NSCalendarServer, Local: "invite"}) {
			if raw, ok := c.inviteProperty(); ok {
				out[n] = raw
			}
			continue
		}
		if v, exists := c.Info.Props[n]; exists {
			out[n] = v
		}
	}
	return out, nil
}

// inviteProperty renders the {http://calendarserver.org/ns/}invite property: a
// <cs:user> per sharee with their address, name, invite status, and access
// level. Port of sabre's SharingPlugin invite serialization. Returns ok=false
// when the backend does not support sharing (the property is then omitted); an
// empty (but supported) roster yields ok=true with no <cs:user> children.
func (c *Calendar) inviteProperty() (davxml.Raw, bool) {
	sharing, ok := c.backend.(SharingSupport)
	if !ok {
		return nil, false
	}
	sharees, err := sharing.Invites(c.Info.ID)
	if err != nil {
		return nil, false
	}
	var b strings.Builder
	for _, s := range sharees {
		// The owner's own row (and any not-shared marker) is not a sharee.
		if s.Access == AccessSharedOwner || s.Access == AccessNotShared {
			continue
		}
		b.WriteString("<cs:user><d:href>" + xmlEscapeText(s.Href) + "</d:href>")
		if s.CommonName != "" {
			b.WriteString("<cs:common-name>" + xmlEscapeText(s.CommonName) + "</cs:common-name>")
		}
		switch s.InviteStatus {
		case InviteAccepted:
			b.WriteString("<cs:invite-accepted/>")
		case InviteDeclined:
			b.WriteString("<cs:invite-declined/>")
		case InviteInvalid:
			b.WriteString("<cs:invite-invalid/>")
		default:
			b.WriteString("<cs:invite-noresponse/>")
		}
		b.WriteString("<cs:access>")
		if s.Access == AccessReadWrite {
			b.WriteString("<cs:read-write/>")
		} else {
			b.WriteString("<cs:read/>")
		}
		b.WriteString("</cs:access></cs:user>")
	}
	return davxml.Raw(b.String()), true
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
		// {DAV:}share is a sibling of write (not aggregated by it), so
		// write access alone does not grant re-sharing — like sabre's
		// Sharing\Plugin::getSupportedPrivilegeSet.
		{Name: "{DAV:}share"},
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
	if c.PerUserProps {
		// CS read delegates may write (per-user) properties, never data.
		aces = append(aces, acl.ACE{Privilege: "{DAV:}write-properties",
			Principal: owner + "/calendar-proxy-read", Protected: true})
	}
	// {DAV:}share is granted only on the owner's own view (not-shared or
	// shared-owner), so a sharee cannot re-share, like sabre.
	if c.Info.ShareAccess == 0 || c.Info.ShareAccess == 1 {
		aces = append(aces, acl.ACE{Privilege: "{DAV:}share", Principal: owner, Protected: true})
	}
	return aces
}

// CalendarObject is a single stored event/todo. Port of sabre's
// CalDAV\CalendarObject.
type CalendarObject struct {
	backend  Backend
	calendar CalendarInfo
	obj      ObjectInfo
	view     []byte // memoized per-view payload (see viewData); nil until first served
}

// NewCalendarObject wraps object info into a file node.
func NewCalendarObject(backend Backend, calendar CalendarInfo, obj ObjectInfo) *CalendarObject {
	return &CalendarObject{backend: backend, calendar: calendar, obj: obj}
}

func (o *CalendarObject) Name() string { return o.obj.URI }
func (o *CalendarObject) SetName(string) error {
	// Sabre's CalendarObject inherits DAV\Node::setName, which forbids
	// renaming a file; match that exactly (a MOVE-rename gets 403, not 405).
	return dav.ErrForbidden("Permission denied to rename file")
}
func (o *CalendarObject) Delete() error {
	return o.backend.DeleteCalendarObject(o.calendar.ID, o.obj.URI)
}
func (o *CalendarObject) LastModified() time.Time { return time.Time{} }

func (o *CalendarObject) Get() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(o.viewData())), nil
}

// viewData is the object as served for this particular view. On a read-only
// shared view it redacts CLASS:CONFIDENTIAL events to opaque busy blocks,
// enforcing RFC 5545's confidential access class; the owner, the owner's
// proxies, and read-write sharees (who can edit, so must see the real object to
// round-trip it) get the full payload. The read-only view is identified by
// sabre's read-only marker — the same signal the object ACL keys off, and the
// one the storage backend actually populates (ACCESS_READ sets it; the
// ShareAccess field is left zero). The result is memoized because Get, Data,
// and Size each ask for it within one request.
func (o *CalendarObject) viewData() []byte {
	if !(&Calendar{Info: o.calendar}).readOnly() {
		return o.obj.CalendarData
	}
	if o.view == nil {
		o.view = ical.RedactConfidential(o.obj.CalendarData)
	}
	return o.view
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
func (o *CalendarObject) Size() int64  { return int64(len(o.viewData())) }
func (o *CalendarObject) ETag() string { return o.obj.QuotedETag() }

// Data exposes the iCalendar payload as served for this view (confidential
// events redacted on a read-only shared view). The REPORT/PROPFIND calendar-data
// paths read through here, so redaction applies to them as well as to GET.
func (o *CalendarObject) Data() []byte { return o.viewData() }

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
