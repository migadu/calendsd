package caldav

import (
	"bytes"
	"context"
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
func (r *CalendarRoot) SetName(context.Context, string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (r *CalendarRoot) Delete(ctx context.Context) error {
	return dav.ErrForbidden("Permission denied to delete node")
}
func (r *CalendarRoot) LastModified() time.Time { return time.Time{} }

func (r *CalendarRoot) Children(ctx context.Context) ([]dav.Node, error) {
	principals, err := r.principals.PrincipalsByPrefix(ctx, r.principalPrefix)
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

func (r *CalendarRoot) Child(ctx context.Context, name string) (dav.Node, error) {
	p, err := r.principals.PrincipalByPath(ctx, r.principalPrefix+"/"+name)
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

func (r *CalendarRoot) ChildExists(ctx context.Context, name string) bool {
	p, err := r.principals.PrincipalByPath(ctx, r.principalPrefix+"/"+name)
	return err == nil && p != nil
}

func (r *CalendarRoot) CreateFile(context.Context, string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in this collection is not supported")
}

func (r *CalendarRoot) CreateDirectory(context.Context, string) error {
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

func (h *CalendarHome) SetName(context.Context, string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (h *CalendarHome) Delete(ctx context.Context) error {
	return dav.ErrForbidden("Permission denied to delete node")
}
func (h *CalendarHome) LastModified() time.Time { return time.Time{} }

func (h *CalendarHome) Children(ctx context.Context) ([]dav.Node, error) {
	infos, err := h.provisioned(ctx)
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

func (h *CalendarHome) Child(ctx context.Context, name string) (dav.Node, error) {
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
	infos, err := h.provisioned(ctx)
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
func (h *CalendarHome) provisioned(ctx context.Context) ([]CalendarInfo, error) {
	infos, err := h.backend.CalendarsForUser(ctx, h.principalURI)
	if err != nil || !h.autoProvision || len(infos) > 0 {
		return infos, err
	}
	if _, err := h.backend.CreateCalendar(ctx, h.principalURI, "calendar", map[xml.Name]any{
		{Space: davxml.NSDav, Local: "displayname"}: "calendar",
	}); err != nil {
		return nil, err
	}
	return h.backend.CalendarsForUser(ctx, h.principalURI)
}

func (h *CalendarHome) ChildExists(ctx context.Context, name string) bool {
	_, err := h.Child(ctx, name)
	return err == nil
}

func (h *CalendarHome) CreateFile(context.Context, string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating new files in this collection is not supported")
}

// CreateDirectory refuses plain MKCOL; calendars come via MKCALENDAR (the
// plugin calls CreateCalendar).
func (h *CalendarHome) CreateDirectory(context.Context, string) error {
	return dav.ErrMethodNotAllowed("Creating new collections in this collection is not supported")
}

// CreateCalendar makes a new calendar in this home (extended MKCOL and
// MKCALENDAR land here).
func (h *CalendarHome) CreateCalendar(ctx context.Context, name string, props map[xml.Name]any) error {
	_, err := h.backend.CreateCalendar(ctx, h.principalURI, name, props)
	return err
}

// CreateExtendedCollection implements dav.ExtendedCollection: calendars and
// subscriptions, depending on the resource types. Port of sabre's
// CalendarHome::createExtendedCollection.
func (h *CalendarHome) CreateExtendedCollection(ctx context.Context, name string, resourceTypes []xml.Name, props map[xml.Name]any) error {
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
		_, err := subs.CreateSubscription(ctx, h.principalURI, name, props)
		return err
	case isCalendar:
		return h.CreateCalendar(ctx, name, props)
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
func (c *Calendar) SetName(context.Context, string) error {
	return dav.ErrMethodNotAllowed("Renaming calendars is not yet supported")
}
func (c *Calendar) Delete(ctx context.Context) error {
	return c.backend.DeleteCalendar(ctx, c.Info.ID)
}
func (c *Calendar) LastModified() time.Time { return time.Time{} }

func (c *Calendar) Children(ctx context.Context) ([]dav.Node, error) {
	objs, err := c.backend.CalendarObjects(ctx, c.Info.ID)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, obj := range objs {
		out = append(out, NewCalendarObject(c.backend, c.Info, obj))
	}
	return out, nil
}

func (c *Calendar) Child(ctx context.Context, name string) (dav.Node, error) {
	obj, err := c.backend.CalendarObject(ctx, c.Info.ID, name)
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
func (c *Calendar) QueryObjects(ctx context.Context, componentType string, start, end time.Time) ([]*CalendarObject, error) {
	uris, err := c.backend.CalendarQuery(ctx, c.Info.ID, componentType, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]*CalendarObject, 0, len(uris))
	for _, uri := range uris {
		obj, err := c.backend.CalendarObject(ctx, c.Info.ID, uri)
		if err != nil {
			return nil, err
		}
		if obj != nil {
			out = append(out, NewCalendarObject(c.backend, c.Info, *obj))
		}
	}
	return out, nil
}

// ObjectByUID returns the calendar's object carrying uid, or nil. A backend
// implementing UIDLookup answers from its index; otherwise every object is
// loaded and parsed.
func (c *Calendar) ObjectByUID(ctx context.Context, uid string) (*CalendarObject, error) {
	if lookup, ok := c.backend.(UIDLookup); ok {
		obj, err := lookup.CalendarObjectByUID(ctx, c.Info.ID, uid)
		if err != nil || obj == nil {
			return nil, err
		}
		return NewCalendarObject(c.backend, c.Info, *obj), nil
	}
	objs, err := c.backend.CalendarObjects(ctx, c.Info.ID)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if d, err := ical.Denormalize(obj.CalendarData); err == nil && d.UID == uid {
			return NewCalendarObject(c.backend, c.Info, obj), nil
		}
	}
	return nil, nil
}

func (c *Calendar) ChildExists(ctx context.Context, name string) bool {
	obj, err := c.backend.CalendarObject(ctx, c.Info.ID, name)
	return err == nil && obj != nil
}

func (c *Calendar) CreateFile(ctx context.Context, name string, data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	return c.backend.CreateCalendarObject(ctx, c.Info.ID, name, raw)
}

func (c *Calendar) CreateDirectory(context.Context, string) error {
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
func (c *Calendar) Props(ctx context.Context, names []xml.Name) (map[xml.Name]any, error) {
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
			if raw, ok := c.inviteProperty(ctx); ok {
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
func (c *Calendar) inviteProperty(ctx context.Context) (davxml.Raw, bool) {
	sharing, ok := c.backend.(SharingSupport)
	if !ok {
		return nil, false
	}
	sharees, err := sharing.Invites(ctx, c.Info.ID)
	if err != nil {
		return nil, false
	}
	var users []inviteUserXML
	for _, s := range sharees {
		// The owner's own row (and any not-shared marker) is not a sharee.
		if s.Access == AccessSharedOwner || s.Access == AccessNotShared {
			continue
		}
		users = append(users, inviteUserXML{
			Href: s.Href, CommonName: s.CommonName,
			Status: statusXML(s.InviteStatus), Access: accessXML(s.Access),
		})
	}
	raw, err := xml.Marshal(users)
	if err != nil {
		return nil, false
	}
	return davxml.Raw(raw), true
}

// inviteUserXML is one sharee of the {cs}invite property.
type inviteUserXML struct {
	XMLName    xml.Name `xml:"http://calendarserver.org/ns/ user"`
	Href       string   `xml:"DAV: href"`
	CommonName string   `xml:"http://calendarserver.org/ns/ common-name,omitempty"`
	Status     inviteStatusXML
	Access     shareAccessXML `xml:"http://calendarserver.org/ns/ access"`
}

func (c *Calendar) PropPatch(ctx context.Context, pp *dav.PropPatch) {
	c.backend.UpdateCalendar(ctx, c.Info.ID, pp)
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
func (c *Calendar) Changes(ctx context.Context, syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	return c.backend.ChangesForCalendar(ctx, c.Info.ID, syncToken, syncLevel, limit)
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

// transparent reports the {caldav}schedule-calendar-transp:transparent marker.
// A transparent calendar is excluded from free-busy aggregation (RFC 6638
// §9.1) — the lever a user flips to make a calendar "private" so its busy
// times stay invisible to others. The stored value is a raw element
// (<cal:transparent/> or <cal:opaque/>); accept the several representations a
// value can take across backends and PROPPATCH round-trips.
func (c *Calendar) transparent() bool {
	v, ok := c.Info.Props[xml.Name{Space: davxml.NSCalDAV, Local: "schedule-calendar-transp"}]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case davxml.Raw:
		return bytes.Contains(t, []byte("transparent"))
	case []byte:
		return bytes.Contains(t, []byte("transparent"))
	case string:
		return strings.Contains(t, "transparent")
	}
	return false
}

// ACL mirrors sabre Calendar::getACL — owner + proxies read, and write only
// when the view is not read-only — except for free-busy: sabre grants
// read-free-busy to {DAV:}authenticated, while we grant it to the owner's
// free-busy audience: their email domain by default, so users cannot probe
// availability across domains, narrowed or extended by the owner's
// free-busy visibility (VAVAILABILITY-PLAN.md §10). Both the free-busy-query
// REPORT and the outbox POST honor it.
func (c *Calendar) ACL() []acl.ACE {
	owner := c.Owner()
	aces := []acl.ACE{
		{Privilege: "{DAV:}read", Principal: owner, Protected: true},
		{Privilege: "{DAV:}read", Principal: owner + "/calendar-proxy-write", Protected: true},
		{Privilege: "{DAV:}read", Principal: owner + "/calendar-proxy-read", Protected: true},
		{Privilege: "{urn:ietf:params:xml:ns:caldav}read-free-busy", Principal: acl.FreeBusyAudience, Protected: true},
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
	if c.Info.ShareAccess == AccessNotShared || c.Info.ShareAccess == AccessSharedOwner {
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
func (o *CalendarObject) SetName(context.Context, string) error {
	// Sabre's CalendarObject inherits DAV\Node::setName, which forbids
	// renaming a file; match that exactly (a MOVE-rename gets 403, not 405).
	return dav.ErrForbidden("Permission denied to rename file")
}
func (o *CalendarObject) Delete(ctx context.Context) error {
	return o.backend.DeleteCalendarObject(ctx, o.calendar.ID, o.obj.URI)
}
func (o *CalendarObject) LastModified() time.Time { return time.Time{} }

func (o *CalendarObject) Get(ctx context.Context) (io.ReadCloser, error) {
	return dav.BytesBody(o.viewData()), nil
}

// viewData is the object as served for this particular view. On a read-only
// shared view it redacts CLASS:CONFIDENTIAL and CLASS:PRIVATE events to opaque
// busy blocks, enforcing RFC 5545's restricted access classes; the owner, the
// owner's proxies, and read-write sharees (who can edit, so must see the real
// object to round-trip it) get the full payload. The read-only view is
// identified by sabre's read-only marker — the same signal the object ACL keys
// off (storage sets it for ACCESS_READ). The result is memoized because Get,
// Data, and Size each ask for it within one request.
func (o *CalendarObject) viewData() []byte {
	if !(&Calendar{Info: o.calendar}).readOnly() {
		return o.obj.CalendarData
	}
	if o.view == nil {
		o.view = ical.RedactRestricted(o.obj.CalendarData)
	}
	return o.view
}

func (o *CalendarObject) Put(ctx context.Context, data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	etag, err := o.backend.UpdateCalendarObject(ctx, o.calendar.ID, o.obj.URI, raw)
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
