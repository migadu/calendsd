package caldav

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// InviteNotification is a share invitation delivered to a sharee's
// notification inbox. Port of the fields sabre's Xml\Notification\Invite
// serializes.
type InviteNotification struct {
	// ID is the notification's stable identifier (also its file name stem).
	ID string
	// DTStamp is when the invite was created.
	DTStamp time.Time
	// HostURL is the shared calendar's path (relative to base).
	HostURL string
	// Organizer is the sharer's principal path.
	Organizer     string
	OrganizerName string
	// Access is AccessRead or AccessReadWrite.
	Access int
	// InviteStatus is one of the Invite* constants.
	InviteStatus int
	// Summary is the optional invitation message.
	Summary string
}

// NotificationsBackend stores per-principal notifications. Port of sabre's
// Backend\NotificationSupport (invite subset).
type NotificationsBackend interface {
	NotificationsForPrincipal(principalURI string) ([]InviteNotification, error)
	AddNotification(principalURI string, n InviteNotification) error
	DeleteNotification(principalURI, id string) error
}

// Notifications is a principal's notification collection. Port of
// CalDAV\Notifications\Collection.
type Notifications struct {
	backend      NotificationsBackend
	principalURI string
}

// NewNotifications builds the notification collection for a principal.
func NewNotifications(backend NotificationsBackend, principalURI string) *Notifications {
	return &Notifications{backend: backend, principalURI: principalURI}
}

func (n *Notifications) Name() string { return "notifications" }
func (n *Notifications) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (n *Notifications) Delete() error           { return dav.ErrForbidden("Permission denied to delete node") }
func (n *Notifications) LastModified() time.Time { return time.Time{} }
func (n *Notifications) Owner() string           { return n.principalURI }

func (n *Notifications) Children() ([]dav.Node, error) {
	items, err := n.backend.NotificationsForPrincipal(n.principalURI)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, item := range items {
		out = append(out, &NotificationNode{backend: n.backend, principalURI: n.principalURI, invite: item})
	}
	return out, nil
}

func (n *Notifications) Child(name string) (dav.Node, error) {
	id := strings.TrimSuffix(name, ".xml")
	items, err := n.backend.NotificationsForPrincipal(n.principalURI)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID == id {
			return &NotificationNode{backend: n.backend, principalURI: n.principalURI, invite: item}, nil
		}
	}
	return nil, dav.ErrNotFound("Notification not found")
}

func (n *Notifications) ChildExists(name string) bool {
	_, err := n.Child(name)
	return err == nil
}

func (n *Notifications) CreateFile(string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in the notifications collection is not allowed")
}
func (n *Notifications) CreateDirectory(string) error {
	return dav.ErrMethodNotAllowed("Creating collections in the notifications collection is not allowed")
}

// ACL grants the owner full access.
func (n *Notifications) ACL() []acl.ACE {
	return []acl.ACE{{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true}}
}

// NotificationNode is one notification item (an invite). Port of
// CalDAV\Notifications\Node.
type NotificationNode struct {
	backend      NotificationsBackend
	principalURI string
	invite       InviteNotification
}

func (nn *NotificationNode) Name() string { return nn.invite.ID + ".xml" }

// Owner returns the principal whose notification inbox holds this item.
func (nn *NotificationNode) Owner() string { return nn.principalURI }

// ACL restricts the notification to its owner, mirroring the parent
// Notifications collection. Without this the node has no ACLHolder and falls
// back to the default {DAV:}all-for-authenticated grant, leaking share
// invitations cross-user.
func (nn *NotificationNode) ACL() []acl.ACE {
	return []acl.ACE{{Privilege: "{DAV:}all", Principal: "{DAV:}owner", Protected: true}}
}

func (nn *NotificationNode) SetName(string) error {
	return dav.ErrMethodNotAllowed("Renaming notifications is not allowed")
}
func (nn *NotificationNode) Delete() error {
	return nn.backend.DeleteNotification(nn.principalURI, nn.invite.ID)
}
func (nn *NotificationNode) LastModified() time.Time { return nn.invite.DTStamp }
func (nn *NotificationNode) ContentType() string     { return "application/xml" }

func (nn *NotificationNode) body(baseURI string) []byte {
	cs := "http://calendarserver.org/ns/"
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<cs:notification xmlns:d="DAV:" xmlns:cs="` + cs + `">` + "\n")
	b.WriteString("  <cs:dtstamp>" + nn.invite.DTStamp.UTC().Format("20060102T150405Z") + "</cs:dtstamp>\n")
	b.WriteString("  <cs:invite-notification>\n")
	b.WriteString("    <cs:uid>" + xmlEsc(nn.invite.ID) + "</cs:uid>\n")
	b.WriteString("    <d:href>" + xmlEsc(baseURI+nn.principalURI+"/") + "</d:href>\n")
	switch nn.invite.InviteStatus {
	case InviteAccepted:
		b.WriteString("    <cs:invite-accepted/>\n")
	case InviteDeclined:
		b.WriteString("    <cs:invite-declined/>\n")
	default:
		b.WriteString("    <cs:invite-noresponse/>\n")
	}
	b.WriteString("    <cs:hosturl><d:href>" + xmlEsc(baseURI+nn.invite.HostURL) + "</d:href></cs:hosturl>\n")
	if nn.invite.Summary != "" {
		b.WriteString("    <cs:summary>" + xmlEsc(nn.invite.Summary) + "</cs:summary>\n")
	}
	b.WriteString("    <cs:access>")
	if nn.invite.Access == AccessReadWrite {
		b.WriteString("<cs:read-write/>")
	} else {
		b.WriteString("<cs:read/>")
	}
	b.WriteString("</cs:access>\n")
	b.WriteString("    <cs:organizer><d:href>" + xmlEsc(baseURI+nn.invite.Organizer+"/") + "</d:href>")
	if nn.invite.OrganizerName != "" {
		b.WriteString("<cs:common-name>" + xmlEsc(nn.invite.OrganizerName) + "</cs:common-name>")
	}
	b.WriteString("</cs:organizer>\n")
	b.WriteString("  </cs:invite-notification>\n")
	b.WriteString("</cs:notification>\n")
	return b.Bytes()
}

// storedNotificationGet is served by the GetHandlerHook in the plugin, which
// has the base URI. NotificationNode itself carries the data.

func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// MemoryNotificationsBackend is an in-memory notification store.
type MemoryNotificationsBackend struct {
	// Notifications maps principal → items.
	Notifications map[string][]InviteNotification
}

func (m *MemoryNotificationsBackend) NotificationsForPrincipal(principalURI string) ([]InviteNotification, error) {
	return m.Notifications[principalURI], nil
}

func (m *MemoryNotificationsBackend) AddNotification(principalURI string, n InviteNotification) error {
	if m.Notifications == nil {
		m.Notifications = map[string][]InviteNotification{}
	}
	m.Notifications[principalURI] = append(m.Notifications[principalURI], n)
	return nil
}

func (m *MemoryNotificationsBackend) DeleteNotification(principalURI, id string) error {
	items := m.Notifications[principalURI]
	for i, item := range items {
		if item.ID == id {
			m.Notifications[principalURI] = append(items[:i], items[i+1:]...)
			return nil
		}
	}
	return nil
}

// notificationID derives a stable id from the calendar and sharee.
func notificationID(calendarPath, sharee string) string {
	sum := md5.Sum([]byte(calendarPath + "|" + sharee))
	return hex.EncodeToString(sum[:])
}

// NotificationsPlugin adds the calendarserver notification inbox: the
// notification-URL principal property and GET of notification items. Port of
// the CalDAV\Notifications\Plugin subset. The inbox is exposed as a
// CalendarHome child when the calendar backend implements
// NotificationsBackend.
type NotificationsPlugin struct {
	server *dav.Server
}

func (p *NotificationsPlugin) PluginName() string { return "notifications" }
func (p *NotificationsPlugin) Init(s *dav.Server) { p.server = s }
func (p *NotificationsPlugin) Features() []string { return []string{"notification"} }

// PropFind provides {cs}notification-URL on principals.
func (p *NotificationsPlugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	if ph, ok := node.(acl.PrincipalHolder); ok {
		pf.Handle(xml.Name{Space: davxml.NSCalendarServer, Local: "notification-URL"}, func() any {
			_, name := splitPath(ph.PrincipalURL())
			return davxml.Href{p.server.BaseURI + "calendars/" + name + "/notifications/"}
		})
	}
	return nil
}

// HandleGet serves a notification item's XML body.
func (p *NotificationsPlugin) HandleGet(node dav.Node, w http.ResponseWriter, r *http.Request) (bool, error) {
	nn, ok := node.(*NotificationNode)
	if !ok {
		return false, nil
	}
	body := nn.body(p.server.BaseURI)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(body)
	return true, err
}
