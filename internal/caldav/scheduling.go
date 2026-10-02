package caldav

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"io"
	"time"

	"calserver/internal/acl"
	"calserver/internal/dav"
)

// SchedulingObjectInfo is one message in a principal's schedule inbox.
type SchedulingObjectInfo struct {
	URI          string
	PrincipalURI string
	CalendarData []byte
}

// SchedulingBackend stores schedule inbox messages (the schedulingobjects
// table). Port of sabre's Backend\SchedulingSupport.
type SchedulingBackend interface {
	SchedulingObjects(principalURI string) ([]SchedulingObjectInfo, error)
	SchedulingObject(principalURI, objectURI string) (*SchedulingObjectInfo, error)
	CreateSchedulingObject(principalURI, objectURI string, data []byte) error
	DeleteSchedulingObject(principalURI, objectURI string) error
}

// AvailabilityBackend stores a principal's working hours: the RFC 7953
// calendar-availability property of their schedule inbox, as one VCALENDAR
// (VAVAILABILITY-PLAN.md §3.1). A scheduling backend that implements it turns
// the property on; Availability returns nil data when none is set.
type AvailabilityBackend interface {
	Availability(principalURI string) ([]byte, error)
	SetAvailability(principalURI string, data []byte) error
	DeleteAvailability(principalURI string) error
}

// FreeBusyVisibilityBackend stores who may see a principal's free-busy
// (VAVAILABILITY-PLAN.md §10). A scheduling backend that implements it turns
// on the inbox's free-busy-visibility property; the same store is what the
// ACL plugin's FreeBusyPolicy reads. FreeBusyVisibility returns the zero
// value (the default) when nothing is set.
type FreeBusyVisibilityBackend interface {
	acl.FreeBusyPolicy
	SetFreeBusyVisibility(principalURI string, v acl.FreeBusyVisibility) error
	DeleteFreeBusyVisibility(principalURI string) error
}

// Inbox is a principal's schedule inbox. Port of Schedule\Inbox.
type Inbox struct {
	backend      SchedulingBackend
	principalURI string
}

// visibility returns the inbox's free-busy visibility store, or nil when
// the backend has none.
func (i *Inbox) visibility() FreeBusyVisibilityBackend {
	vb, _ := i.backend.(FreeBusyVisibilityBackend)
	return vb
}

// availability returns the inbox's working-hours store, or nil when the
// backend has none.
func (i *Inbox) availability() AvailabilityBackend {
	ab, _ := i.backend.(AvailabilityBackend)
	return ab
}

// NewInbox builds a schedule inbox node.
func NewInbox(backend SchedulingBackend, principalURI string) *Inbox {
	return &Inbox{backend: backend, principalURI: principalURI}
}

func (i *Inbox) Name() string            { return "inbox" }
func (i *Inbox) SetName(string) error    { return dav.ErrForbidden("Permission denied to rename node") }
func (i *Inbox) Delete() error           { return dav.ErrForbidden("Permission denied to delete node") }
func (i *Inbox) LastModified() time.Time { return time.Time{} }
func (i *Inbox) Owner() string           { return i.principalURI }
func (i *Inbox) PrincipalURI() string    { return i.principalURI }

func (i *Inbox) Children() ([]dav.Node, error) {
	objs, err := i.backend.SchedulingObjects(i.principalURI)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, obj := range objs {
		out = append(out, &SchedulingObject{backend: i.backend, info: obj})
	}
	return out, nil
}

func (i *Inbox) Child(name string) (dav.Node, error) {
	obj, err := i.backend.SchedulingObject(i.principalURI, name)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, dav.ErrNotFound("Scheduling object not found")
	}
	return &SchedulingObject{backend: i.backend, info: *obj}, nil
}

func (i *Inbox) ChildExists(name string) bool {
	obj, err := i.backend.SchedulingObject(i.principalURI, name)
	return err == nil && obj != nil
}

func (i *Inbox) CreateFile(name string, data io.Reader) (string, error) {
	raw, err := io.ReadAll(data)
	if err != nil {
		return "", err
	}
	return "", i.backend.CreateSchedulingObject(i.principalURI, name, raw)
}

func (i *Inbox) CreateDirectory(string) error {
	return dav.ErrMethodNotAllowed("Creating collections in the inbox is not allowed")
}

// ACL grants the owner full access and authenticated users delivery rights,
// like sabre's Inbox::getACL.
func (i *Inbox) ACL() []acl.ACE {
	return []acl.ACE{
		{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true},
		{Privilege: "{urn:ietf:params:xml:ns:caldav}schedule-deliver-invite", Principal: "{DAV:}authenticated", Protected: true},
		{Privilege: "{urn:ietf:params:xml:ns:caldav}schedule-deliver-reply", Principal: "{DAV:}authenticated", Protected: true},
	}
}

// SupportedPrivileges extends the tree with the schedule-deliver aggregate.
func (i *Inbox) SupportedPrivileges() *acl.Privilege {
	return &acl.Privilege{Name: "{DAV:}all", Abstract: true, Aggregates: []acl.Privilege{
		{Name: "{DAV:}read"},
		// write-properties lets the owner PROPPATCH the inbox's own
		// properties; like sabre's default set, which its
		// Inbox extends with schedule-deliver.
		{Name: "{DAV:}write", Aggregates: []acl.Privilege{
			{Name: "{DAV:}write-properties"}, {Name: "{DAV:}write-content"},
			{Name: "{DAV:}bind"}, {Name: "{DAV:}unbind"},
		}},
		{Name: "{urn:ietf:params:xml:ns:caldav}schedule-deliver", Aggregates: []acl.Privilege{
			{Name: "{urn:ietf:params:xml:ns:caldav}schedule-deliver-invite"},
			{Name: "{urn:ietf:params:xml:ns:caldav}schedule-deliver-reply"},
		}},
	}}
}

// SchedulingObject is one inbox message. Port of Schedule\SchedulingObject.
type SchedulingObject struct {
	backend SchedulingBackend
	info    SchedulingObjectInfo
}

func (s *SchedulingObject) Name() string { return s.info.URI }

// Owner returns the principal whose inbox holds this message.
func (s *SchedulingObject) Owner() string { return s.info.PrincipalURI }

// ACL restricts the message to its owner. Without this the node has no
// ACLHolder and falls back to the default {DAV:}all-for-authenticated grant,
// letting any authenticated user read another principal's inbox iTIP payloads.
func (s *SchedulingObject) ACL() []acl.ACE {
	return []acl.ACE{
		{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true},
	}
}

func (s *SchedulingObject) SetName(string) error {
	return dav.ErrMethodNotAllowed("Renaming scheduling objects is not allowed")
}
func (s *SchedulingObject) Delete() error {
	return s.backend.DeleteSchedulingObject(s.info.PrincipalURI, s.info.URI)
}
func (s *SchedulingObject) LastModified() time.Time { return time.Time{} }
func (s *SchedulingObject) ContentType() string     { return "text/calendar; charset=utf-8" }
func (s *SchedulingObject) Size() int64             { return int64(len(s.info.CalendarData)) }

func (s *SchedulingObject) ETag() string {
	sum := md5.Sum(s.info.CalendarData)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *SchedulingObject) Get() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.info.CalendarData)), nil
}

func (s *SchedulingObject) Put(io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Updating scheduling objects is not allowed")
}

// Outbox is the POST target for outgoing scheduling messages. Port of
// Schedule\Outbox.
type Outbox struct {
	principalURI string
}

// NewOutbox builds a schedule outbox node.
func NewOutbox(principalURI string) *Outbox {
	return &Outbox{principalURI: principalURI}
}

func (o *Outbox) Name() string            { return "outbox" }
func (o *Outbox) SetName(string) error    { return dav.ErrForbidden("Permission denied to rename node") }
func (o *Outbox) Delete() error           { return dav.ErrForbidden("Permission denied to delete node") }
func (o *Outbox) LastModified() time.Time { return time.Time{} }
func (o *Outbox) Owner() string           { return o.principalURI }
func (o *Outbox) PrincipalURI() string    { return o.principalURI }

func (o *Outbox) Children() ([]dav.Node, error) { return nil, nil }
func (o *Outbox) Child(name string) (dav.Node, error) {
	return nil, dav.ErrNotFound("File not found: %s in 'outbox'", name)
}

// ACL restricts the outbox to its owner: only they may read it or send
// scheduling messages from it. Without this the node has no ACLHolder and
// falls back to the default {DAV:}all-for-authenticated grant, which would let
// any authenticated user POST free-busy probes from anyone's outbox.
func (o *Outbox) ACL() []acl.ACE {
	return []acl.ACE{
		{Privilege: "{DAV:}read", Principal: "{DAV:}owner", Protected: true},
		{Privilege: "{urn:ietf:params:xml:ns:caldav}schedule-send", Principal: "{DAV:}owner", Protected: true},
	}
}

// SupportedPrivileges extends the tree with the schedule-send aggregate so the
// owner's schedule-send grant expands to its subtypes.
func (o *Outbox) SupportedPrivileges() *acl.Privilege {
	return &acl.Privilege{Name: "{DAV:}all", Abstract: true, Aggregates: []acl.Privilege{
		{Name: "{DAV:}read"},
		{Name: "{urn:ietf:params:xml:ns:caldav}schedule-send", Aggregates: []acl.Privilege{
			{Name: "{urn:ietf:params:xml:ns:caldav}schedule-send-invite"},
			{Name: "{urn:ietf:params:xml:ns:caldav}schedule-send-reply"},
			{Name: "{urn:ietf:params:xml:ns:caldav}schedule-send-freebusy"},
		}},
	}}
}
func (o *Outbox) ChildExists(string) bool { return false }
func (o *Outbox) CreateFile(string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in the outbox is not allowed")
}
func (o *Outbox) CreateDirectory(string) error {
	return dav.ErrMethodNotAllowed("Creating collections in the outbox is not allowed")
}

// MockSchedulingBackend adds in-memory scheduling storage to the mock
// calendar backend, like sabre's Backend\MockScheduling.
type MockSchedulingBackend struct {
	MockBackend
	// Scheduling maps principal → uri → data.
	Scheduling map[string]map[string][]byte
	// Availabilities maps principal → calendar-availability VCALENDAR.
	Availabilities map[string][]byte
	// Visibilities maps principal → free-busy visibility.
	Visibilities map[string]acl.FreeBusyVisibility
}

func (m *MockSchedulingBackend) FreeBusyVisibility(principalURI string) (acl.FreeBusyVisibility, error) {
	return m.Visibilities[principalURI], nil
}

func (m *MockSchedulingBackend) SetFreeBusyVisibility(principalURI string, v acl.FreeBusyVisibility) error {
	if m.Visibilities == nil {
		m.Visibilities = map[string]acl.FreeBusyVisibility{}
	}
	m.Visibilities[principalURI] = v
	return nil
}

func (m *MockSchedulingBackend) DeleteFreeBusyVisibility(principalURI string) error {
	delete(m.Visibilities, principalURI)
	return nil
}

func (m *MockSchedulingBackend) Availability(principalURI string) ([]byte, error) {
	return m.Availabilities[principalURI], nil
}

func (m *MockSchedulingBackend) SetAvailability(principalURI string, data []byte) error {
	if m.Availabilities == nil {
		m.Availabilities = map[string][]byte{}
	}
	m.Availabilities[principalURI] = data
	return nil
}

func (m *MockSchedulingBackend) DeleteAvailability(principalURI string) error {
	delete(m.Availabilities, principalURI)
	return nil
}

func (m *MockSchedulingBackend) SchedulingObjects(principalURI string) ([]SchedulingObjectInfo, error) {
	var out []SchedulingObjectInfo
	for uri, data := range m.Scheduling[principalURI] {
		out = append(out, SchedulingObjectInfo{URI: uri, PrincipalURI: principalURI, CalendarData: data})
	}
	return out, nil
}

func (m *MockSchedulingBackend) SchedulingObject(principalURI, objectURI string) (*SchedulingObjectInfo, error) {
	data, exists := m.Scheduling[principalURI][objectURI]
	if !exists {
		return nil, nil
	}
	return &SchedulingObjectInfo{URI: objectURI, PrincipalURI: principalURI, CalendarData: data}, nil
}

func (m *MockSchedulingBackend) CreateSchedulingObject(principalURI, objectURI string, data []byte) error {
	if m.Scheduling == nil {
		m.Scheduling = map[string]map[string][]byte{}
	}
	if m.Scheduling[principalURI] == nil {
		m.Scheduling[principalURI] = map[string][]byte{}
	}
	m.Scheduling[principalURI][objectURI] = data
	return nil
}

func (m *MockSchedulingBackend) DeleteSchedulingObject(principalURI, objectURI string) error {
	delete(m.Scheduling[principalURI], objectURI)
	return nil
}
