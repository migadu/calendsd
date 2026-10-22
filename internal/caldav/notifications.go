package caldav

import (
	"context"
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
	"calserver/internal/pathutil"
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
	NotificationsForPrincipal(ctx context.Context, principalURI string) ([]InviteNotification, error)
	AddNotification(ctx context.Context, principalURI string, n InviteNotification) error
	DeleteNotification(ctx context.Context, principalURI, id string) error
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
func (n *Notifications) SetName(context.Context, string) error {
	return dav.ErrForbidden("Permission denied to rename node")
}
func (n *Notifications) Delete(ctx context.Context) error {
	return dav.ErrForbidden("Permission denied to delete node")
}
func (n *Notifications) LastModified() time.Time { return time.Time{} }
func (n *Notifications) Owner() string           { return n.principalURI }

func (n *Notifications) Children(ctx context.Context) ([]dav.Node, error) {
	items, err := n.backend.NotificationsForPrincipal(ctx, n.principalURI)
	if err != nil {
		return nil, err
	}
	var out []dav.Node
	for _, item := range items {
		out = append(out, &NotificationNode{backend: n.backend, principalURI: n.principalURI, invite: item})
	}
	return out, nil
}

func (n *Notifications) Child(ctx context.Context, name string) (dav.Node, error) {
	id := strings.TrimSuffix(name, ".xml")
	items, err := n.backend.NotificationsForPrincipal(ctx, n.principalURI)
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

func (n *Notifications) ChildExists(ctx context.Context, name string) bool {
	_, err := n.Child(ctx, name)
	return err == nil
}

func (n *Notifications) CreateFile(context.Context, string, io.Reader) (string, error) {
	return "", dav.ErrMethodNotAllowed("Creating files in the notifications collection is not allowed")
}
func (n *Notifications) CreateDirectory(context.Context, string) error {
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

func (nn *NotificationNode) SetName(context.Context, string) error {
	return dav.ErrMethodNotAllowed("Renaming notifications is not allowed")
}
func (nn *NotificationNode) Delete(ctx context.Context) error {
	return nn.backend.DeleteNotification(ctx, nn.principalURI, nn.invite.ID)
}
func (nn *NotificationNode) LastModified() time.Time { return nn.invite.DTStamp }
func (nn *NotificationNode) ContentType() string     { return "application/xml" }

func (nn *NotificationNode) body(baseURI string) []byte {
	doc := notificationXML{DTStamp: nn.invite.DTStamp.UTC().Format("20060102T150405Z")}
	doc.Invite = inviteNotificationXML{
		UID:     nn.invite.ID,
		Href:    baseURI + nn.principalURI + "/",
		Status:  statusXML(nn.invite.InviteStatus),
		HostURL: hrefXML{Href: baseURI + nn.invite.HostURL},
		Summary: nn.invite.Summary,
		Access:  accessXML(nn.invite.Access),
	}
	if doc.Invite.Status.Invalid != nil { // a notification is never "invalid"
		doc.Invite.Status = statusXML(InviteNoResponse)
	}
	doc.Invite.Organizer.Href = baseURI + nn.invite.Organizer + "/"
	doc.Invite.Organizer.CommonName = nn.invite.OrganizerName
	out, _ := xml.Marshal(doc) // plain strings only: cannot fail
	return append([]byte(xml.Header), out...)
}

// notificationXML is a stored invite notification as served to the sharee.
type notificationXML struct {
	XMLName xml.Name              `xml:"http://calendarserver.org/ns/ notification"`
	DTStamp string                `xml:"http://calendarserver.org/ns/ dtstamp"`
	Invite  inviteNotificationXML `xml:"http://calendarserver.org/ns/ invite-notification"`
}

type inviteNotificationXML struct {
	UID       string `xml:"http://calendarserver.org/ns/ uid"`
	Href      string `xml:"DAV: href"`
	Status    inviteStatusXML
	HostURL   hrefXML        `xml:"http://calendarserver.org/ns/ hosturl"`
	Summary   string         `xml:"http://calendarserver.org/ns/ summary,omitempty"`
	Access    shareAccessXML `xml:"http://calendarserver.org/ns/ access"`
	Organizer struct {
		Href       string `xml:"DAV: href"`
		CommonName string `xml:"http://calendarserver.org/ns/ common-name,omitempty"`
	} `xml:"http://calendarserver.org/ns/ organizer"`
}

// storedNotificationGet is served by the GetHandlerHook in the plugin, which
// has the base URI. NotificationNode itself carries the data.

// MemoryNotificationsBackend is an in-memory notification store.
type MemoryNotificationsBackend struct {
	// Notifications maps principal → items.
	Notifications map[string][]InviteNotification
}

func (m *MemoryNotificationsBackend) NotificationsForPrincipal(ctx context.Context, principalURI string) ([]InviteNotification, error) {
	return m.Notifications[principalURI], nil
}

func (m *MemoryNotificationsBackend) AddNotification(ctx context.Context, principalURI string, n InviteNotification) error {
	if m.Notifications == nil {
		m.Notifications = map[string][]InviteNotification{}
	}
	m.Notifications[principalURI] = append(m.Notifications[principalURI], n)
	return nil
}

func (m *MemoryNotificationsBackend) DeleteNotification(ctx context.Context, principalURI, id string) error {
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
			_, name := pathutil.Split(ph.PrincipalURL())
			return davxml.Href{p.server.Href("calendars/" + name + "/notifications/")}
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
