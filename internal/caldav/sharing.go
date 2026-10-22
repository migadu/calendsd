package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"calendsd/internal/dav"
	davxml "calendsd/internal/dav/xml"
)

// Share access levels and invite statuses, mirroring sabre's
// DAV\Sharing\Plugin constants (also the calendarinstances.access column).
const (
	AccessNotShared   = 0
	AccessSharedOwner = 1
	AccessRead        = 2
	AccessReadWrite   = 3
	AccessNoAccess    = 4 // removal marker in share requests

	InviteNoResponse = 1
	InviteAccepted   = 2
	InviteDeclined   = 3
	InviteInvalid    = 4
)

// Sharee is one entry of a calendar's invite list. Port of DAV\Xml\Element\Sharee.
type Sharee struct {
	Href         string
	Principal    string
	CommonName   string
	Comment      string
	Access       int
	InviteStatus int
}

// SharingSupport marks backends that implement calendar sharing.
type SharingSupport interface {
	// UpdateInvites applies a {cs}share request and reports the committed
	// result: at most one ShareChange per sharee instance the request leaves
	// in place (removals report nothing). It returns changes only after the
	// write committed.
	UpdateInvites(ctx context.Context, id CalendarID, sharees []Sharee) ([]ShareChange, error)
	Invites(ctx context.Context, id CalendarID) ([]Sharee, error)
}

// ShareChange is one sharee instance UpdateInvites created or updated.
type ShareChange struct {
	Href        string // the address the request used for this sharee
	Principal   string // resolved local principal; "" for an unresolved (invalid) invite
	InstanceID  int64  // the sharee's calendarinstances.id; orders re-shares
	InstanceURI string // the sharee's calendar segment in their own home
	Access      int    // committed access level (AccessRead or AccessReadWrite)
	Comment     string // the request's {cs}summary for this sharee
	// Created is true for a new instance row, and for an unresolved invite
	// that this request turned into a usable local share. An access change or
	// an unchanged re-send is false.
	Created bool
}

// PublishSupport marks backends supporting calendar publishing.
type PublishSupport interface {
	SetPublishStatus(ctx context.Context, id CalendarID, value bool) error
}

// ReplySupport marks backends handling share invite replies.
type ReplySupport interface {
	// ShareReply records the replying principal's accept/decline of the share
	// identified by href (the invite's hosturl) and returns the accepted
	// calendar's path relative to the request (the replying principal's
	// calendar home) — a bare segment, no leading slash, which the caller
	// resolves against the base-carrying request URL — or "" on decline.
	ShareReply(ctx context.Context, principalURI, href string, status int, calendarURI, inReplyTo, summary string) (string, error)
}

// MockSharingBackend ports sabre's Backend\MockSharing (with notification
// support so the sharee round-trip can be exercised).
type MockSharingBackend struct {
	MockBackend
	MemoryNotificationsBackend
	Shares map[CalendarID][]Sharee
}

func (m *MockSharingBackend) UpdateInvites(ctx context.Context, id CalendarID, sharees []Sharee) ([]ShareChange, error) {
	if m.Shares == nil {
		m.Shares = map[CalendarID][]Sharee{}
	}
	var changes []ShareChange
	for _, sharee := range finalSharees(sharees, "") {
		existing := -1
		for i, cur := range m.Shares[id] {
			if sameSharee(cur, sharee) {
				existing = i
			}
		}
		sharee.InviteStatus = InviteNoResponse
		switch {
		case sharee.Access == AccessNoAccess && existing >= 0:
			m.Shares[id] = append(m.Shares[id][:existing], m.Shares[id][existing+1:]...)
		case sharee.Access == AccessNoAccess:
		case existing >= 0:
			created := m.Shares[id][existing].Principal == "" && sharee.Principal != ""
			m.Shares[id][existing] = sharee
			changes = append(changes, m.change(id, sharee, created))
		default:
			m.Shares[id] = append(m.Shares[id], sharee)
			changes = append(changes, m.change(id, sharee, true))
		}
	}
	return changes, nil
}

// change reports a mock share with a stable per-(calendar, sharee) identity;
// the mock has no instance rows, so the id is the sharee's position + 1.
func (m *MockSharingBackend) change(id CalendarID, s Sharee, created bool) ShareChange {
	var n int64
	for i, cur := range m.Shares[id] {
		if sameSharee(cur, s) {
			n = int64(i + 1)
		}
	}
	return ShareChange{
		Href: s.Href, Principal: s.Principal, InstanceID: n,
		InstanceURI: "shared-" + id.Calendar + "-" + strings.TrimPrefix(s.Principal, "principals/"),
		Access:      s.Access, Comment: s.Comment, Created: created,
	}
}

func (m *MockSharingBackend) Invites(ctx context.Context, id CalendarID) ([]Sharee, error) {
	return m.Shares[id], nil
}

func (m *MockSharingBackend) SetPublishStatus(ctx context.Context, id CalendarID, value bool) error {
	return nil
}

// ShareReply mirrors sabre's mock: accepting yields a fixed calendar path.
func (m *MockSharingBackend) ShareReply(ctx context.Context, principalURI, href string, status int, calendarURI, inReplyTo, summary string) (string, error) {
	if status == InviteAccepted {
		return "calendars/blabla/calendar", nil
	}
	return "", nil
}

// SharingPlugin handles the calendarserver sharing POSTs. Port of
// CalDAV\SharingPlugin + the relevant DAV\Sharing\Plugin behavior.
type SharingPlugin struct {
	// Notifier, when non-nil, is told about every sharee a {cs}share request
	// newly granted a usable local share, so it can email them.
	Notifier ShareNotifier
	// Now stamps invite notifications; nil uses time.Now (tests pin it).
	Now func() time.Time

	server *dav.Server
}

func (p *SharingPlugin) PluginName() string { return "sharing" }

func (p *SharingPlugin) Init(s *dav.Server) { p.server = s }

func (p *SharingPlugin) Features() []string {
	return []string{"calendarserver-sharing"}
}

func (p *SharingPlugin) Methods(ctx context.Context, path string) []string { return []string{"POST"} }

func (p *SharingPlugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != "POST" {
		return false, nil
	}
	contentType := r.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/xml") && !strings.Contains(contentType, "text/xml") {
		return false, nil
	}
	path, err := p.server.Path(r)
	if err != nil {
		return false, nil
	}
	node, err := p.server.Tree.NodeForPath(r.Context(), path)
	if err != nil {
		return false, nil
	}
	// Only calendars and calendar homes take sharing POSTs; leave every other
	// body unread for the plugins after this one.
	switch node.(type) {
	case *Calendar, *CalendarHome:
	default:
		return false, nil
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return true, err
	}
	// A body that is not a sharing document belongs to a later plugin, which
	// reads r.Body itself: hand it back intact.
	r.Body = io.NopCloser(bytes.NewReader(body))
	root, err := rootName(body)
	if err != nil {
		return true, dav.ErrBadRequest("%s", err)
	}

	cs := func(local string) xml.Name {
		return xml.Name{Space: davxml.NSCalendarServer, Local: local}
	}
	switch root {
	case cs("share"):
		return true, p.shareRequest(w, r, path, node, body)
	case cs("invite-reply"):
		home, isHome := node.(*CalendarHome)
		if !isHome {
			return false, nil
		}
		return true, p.inviteReply(w, r, path, home, body)
	case cs("publish-calendar"):
		return p.publish(w, r, path, node, true)
	case cs("unpublish-calendar"):
		return p.publish(w, r, path, node, false)
	}
	return false, nil
}

// shareRequest applies a {cs}share document to a shareable calendar.
func (p *SharingPlugin) shareRequest(w http.ResponseWriter, r *http.Request, calPath string, node dav.Node, body []byte) error {
	calNode, isCal := node.(*Calendar)
	if !isCal {
		return dav.ErrForbidden("Sharing is not allowed on this node")
	}
	// Sharing requires the {DAV:}share privilege — granted only on the
	// owner's own view, so a sharee's POST yields NeedPrivileges (403).
	if err := p.checkPrivilege(r, calPath, "{DAV:}share"); err != nil {
		return err
	}
	sharing, supported := calNode.backend.(SharingSupport)
	if !supported {
		return dav.ErrForbidden("Sharing is not allowed on this node")
	}

	sharees, err := parseShareBody(body)
	if err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	// Resolve local principals by calendar user address.
	for i := range sharees {
		sharees[i].Principal = principalForAddress(r.Context(), p.server, sharees[i].Href)
	}
	changes, err := sharing.UpdateInvites(r.Context(), calNode.Info.ID, sharees)
	if err != nil {
		return err
	}
	// Drop an invite-notification into the inbox of each local sharee the
	// committed result keeps, so their client's notification feed surfaces
	// the share (like sabre). The request's raw entries are not used: a set
	// overridden by a later remove, or a duplicate spelling, did not share
	// anything. The share has committed, so a failed notification is logged
	// rather than failing the request.
	if notif, ok := calNode.backend.(NotificationsBackend); ok {
		for _, c := range changes {
			if c.Principal == "" || c.Principal == calNode.Owner() {
				continue
			}
			err := notif.AddNotification(r.Context(), c.Principal, InviteNotification{
				ID:           notificationID(calPath, c.Href),
				DTStamp:      p.now(),
				HostURL:      calPath,
				Organizer:    calNode.Owner(),
				Access:       c.Access,
				InviteStatus: InviteNoResponse,
				Summary:      c.Comment,
			})
			if err != nil {
				slog.Warn("share invite notification not stored",
					"calendar", calPath, "principal", c.Principal, "err", err)
			}
		}
	}
	// Email each newly usable local sharee (best effort, never fails the
	// share). The notifier applies the owner/resource exclusions and the
	// recipient domain's policy.
	if p.Notifier != nil {
		for _, c := range changes {
			if !c.Created || c.Principal == "" || c.Principal == calNode.Owner() {
				continue
			}
			p.Notifier.NotifyShare(r, ShareNotice{
				CalendarID:     calNode.Info.ID.Calendar,
				CalendarPath:   calPath,
				OwnerPrincipal: calNode.Owner(),
				Principal:      c.Principal,
				InstanceID:     c.InstanceID,
				InstanceURI:    c.InstanceURI,
				Comment:        c.Comment,
			})
		}
	}
	w.Header().Set("X-Sabre-Status", "everything-went-well")
	w.WriteHeader(http.StatusOK)
	return nil
}

// finalSharees reduces a share request to its final effect per sharee, in
// first-seen order: the last entry for a sharee wins, so a remove (parsed
// after every set) beats a set of the same sharee in the same POST. A
// resolved sharee is identified by principal — so address spellings and
// aliases collapse — and an unresolved one by its address. Entries for the
// owner are dropped: sharing a calendar with its owner means nothing.
func finalSharees(sharees []Sharee, owner string) []Sharee {
	var order []string
	last := map[string]Sharee{}
	for _, s := range sharees {
		if owner != "" && s.Principal == owner {
			continue
		}
		k := shareeKey(s)
		if _, seen := last[k]; !seen {
			order = append(order, k)
		}
		last[k] = s
	}
	out := make([]Sharee, 0, len(order))
	for _, k := range order {
		out = append(out, last[k])
	}
	return out
}

func shareeKey(s Sharee) string {
	if s.Principal != "" {
		return "p:" + s.Principal
	}
	return "h:" + strings.ToLower(s.Href)
}

// sameSharee reports whether an existing roster entry is the sharee s names:
// the same principal, or the same address for an entry not yet bound to a
// principal — or for an s that doesn't resolve (its address still names the
// entry it was shared under).
func sameSharee(cur, s Sharee) bool {
	if s.Principal != "" && cur.Principal == s.Principal {
		return true
	}
	return (cur.Principal == "" || s.Principal == "") && strings.EqualFold(cur.Href, s.Href)
}

// FinalSharees exposes finalSharees to backends outside this package, which
// must apply the same normalization inside their own transaction.
func FinalSharees(sharees []Sharee, owner string) []Sharee { return finalSharees(sharees, owner) }

func (p *SharingPlugin) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// checkPrivilege enforces priv on the target path via the ACL plugin. POST is
// not covered by the ACL BeforeMethod table, so the sharing actions must check
// explicitly to keep a non-owner from mutating another principal's state.
// The right privilege depends on the target node: publish/unpublish and share
// act on a Calendar (owner-only {DAV:}share), while invite-reply acts on the
// sharee's own CalendarHome, which grants {DAV:}write (not {DAV:}share) to its
// owner.
func (p *SharingPlugin) checkPrivilege(r *http.Request, path, priv string) error {
	checker, err := RequirePrivilegeChecker(p.server)
	if err != nil {
		return err
	}
	return checker.CheckPrivileges(r.Context(), path, priv)
}

// inviteReply processes a sharee's accept/decline. It targets the sharee's own
// CalendarHome, so it requires {DAV:}write there (owner and write-proxies), not
// the calendar-level {DAV:}share — the home never grants {DAV:}share.
func (p *SharingPlugin) inviteReply(w http.ResponseWriter, r *http.Request, path string, home *CalendarHome, body []byte) error {
	if err := p.checkPrivilege(r, path, "{DAV:}write"); err != nil {
		return err
	}
	reply, err := parseInviteReply(body)
	if err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	replies, supported := home.backend.(ReplySupport)
	if !supported {
		return dav.ErrNotImplemented("Sharing is not implemented on this backend")
	}
	url, err := replies.ShareReply(r.Context(), home.principalURI, reply.href, reply.status, reply.calendarURI, reply.inReplyTo, reply.summary)
	if err != nil {
		return err
	}
	w.Header().Set("X-Sabre-Status", "everything-went-well")
	if url != "" {
		// sabre resolves the returned href relative to the request URL
		// (LocalHref with contextUri = request path), so a home at
		// /calendars/user1 yields /calendars/<returned-path>. The request path
		// already carries the BaseURI, so a request-relative return keeps the
		// prefix — an absolute (leading-slash) return would drop it.
		body, err := xml.Marshal(sharedAsXML{Href: resolveHref(r.URL.Path, url)})
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(append([]byte(xml.Header), body...))
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (p *SharingPlugin) publish(w http.ResponseWriter, r *http.Request, path string, node dav.Node, value bool) (bool, error) {
	calNode, isCal := node.(*Calendar)
	if !isCal {
		return false, nil
	}
	// Publishing changes the calendar's sharing state, so it requires the
	// {DAV:}share privilege (owner-only) just like shareRequest.
	if err := p.checkPrivilege(r, path, "{DAV:}share"); err != nil {
		return true, err
	}
	publisher, supported := calNode.backend.(PublishSupport)
	if !supported {
		return false, nil
	}
	if err := publisher.SetPublishStatus(r.Context(), calNode.Info.ID, value); err != nil {
		return true, err
	}
	w.Header().Set("X-Sabre-Status", "everything-went-well")
	if value {
		// iCloud sends back the 202, so sabre (and we) do too.
		w.WriteHeader(http.StatusAccepted)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	return true, nil
}

// sharedAsXML is the {cs}shared-as answer to an accepted invite.
type sharedAsXML struct {
	XMLName xml.Name `xml:"http://calendarserver.org/ns/ shared-as"`
	Href    string   `xml:"DAV: href"`
}

// hrefXML is an element whose only child is a {DAV:}href.
type hrefXML struct {
	Href string `xml:"DAV: href"`
}

// shareAccessXML is a {cs}access element: read-write or read.
type shareAccessXML struct {
	ReadWrite *struct{} `xml:"http://calendarserver.org/ns/ read-write"`
	Read      *struct{} `xml:"http://calendarserver.org/ns/ read"`
}

func accessXML(access int) shareAccessXML {
	if access == AccessReadWrite {
		return shareAccessXML{ReadWrite: &struct{}{}}
	}
	return shareAccessXML{Read: &struct{}{}}
}

// inviteStatusXML holds the one {cs}invite-* element naming an invite status.
type inviteStatusXML struct {
	Accepted   *struct{} `xml:"http://calendarserver.org/ns/ invite-accepted"`
	Declined   *struct{} `xml:"http://calendarserver.org/ns/ invite-declined"`
	Invalid    *struct{} `xml:"http://calendarserver.org/ns/ invite-invalid"`
	NoResponse *struct{} `xml:"http://calendarserver.org/ns/ invite-noresponse"`
}

func statusXML(status int) inviteStatusXML {
	switch status {
	case InviteAccepted:
		return inviteStatusXML{Accepted: &struct{}{}}
	case InviteDeclined:
		return inviteStatusXML{Declined: &struct{}{}}
	case InviteInvalid:
		return inviteStatusXML{Invalid: &struct{}{}}
	}
	return inviteStatusXML{NoResponse: &struct{}{}}
}

type shareXML struct {
	XMLName xml.Name `xml:"http://calendarserver.org/ns/ share"`
	Set     []struct {
		Href       string    `xml:"DAV: href"`
		CommonName string    `xml:"common-name"`
		Summary    string    `xml:"summary"`
		ReadWrite  *struct{} `xml:"read-write"`
	} `xml:"set"`
	Remove []struct {
		Href string `xml:"DAV: href"`
	} `xml:"remove"`
}

func parseShareBody(body []byte) ([]Sharee, error) {
	var parsed shareXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&parsed); err != nil {
		return nil, err
	}
	var out []Sharee
	for _, set := range parsed.Set {
		access := AccessRead
		if set.ReadWrite != nil {
			access = AccessReadWrite
		}
		out = append(out, Sharee{
			Href: set.Href, CommonName: set.CommonName,
			Comment: set.Summary, Access: access,
		})
	}
	for _, rem := range parsed.Remove {
		out = append(out, Sharee{Href: rem.Href, Access: AccessNoAccess})
	}
	return out, nil
}

type inviteReplyXML struct {
	XMLName   xml.Name  `xml:"http://calendarserver.org/ns/ invite-reply"`
	HostHref  string    `xml:"hosturl>href"`
	InReplyTo string    `xml:"in-reply-to"`
	Summary   string    `xml:"summary"`
	Accepted  *struct{} `xml:"invite-accepted"`
	Declined  *struct{} `xml:"invite-declined"`
}

type inviteReply struct {
	href, calendarURI, inReplyTo, summary string
	status                                int
}

func parseInviteReply(body []byte) (*inviteReply, error) {
	var parsed inviteReplyXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&parsed); err != nil {
		return nil, err
	}
	if parsed.HostHref == "" {
		return nil, dav.ErrBadRequest("The {http://calendarserver.org/ns/}hosturl element is required")
	}
	status := InviteDeclined
	if parsed.Accepted != nil {
		status = InviteAccepted
	}
	return &inviteReply{
		href: parsed.HostHref, inReplyTo: parsed.InReplyTo,
		summary: parsed.Summary, status: status,
	}, nil
}

// resolveHref resolves a relative href against a request path, replacing the
// last path segment (RFC 3986 relative resolution, as sabre's LocalHref does).
func resolveHref(requestPath, href string) string {
	if strings.HasPrefix(href, "/") {
		return href
	}
	base := requestPath
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[:i+1]
	}
	return base + href
}

func rootName(body []byte) (xml.Name, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.Name{}, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name, nil
		}
	}
}
