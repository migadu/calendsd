package caldav

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
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
	UpdateInvites(id CalendarID, sharees []Sharee) error
	Invites(id CalendarID) ([]Sharee, error)
}

// PublishSupport marks backends supporting calendar publishing.
type PublishSupport interface {
	SetPublishStatus(id CalendarID, value bool) error
}

// ReplySupport marks backends handling share invite replies.
type ReplySupport interface {
	// ShareReply returns the path of the accepted calendar, or "".
	ShareReply(href string, status int, calendarURI, inReplyTo, summary string) (string, error)
}

// MockSharingBackend ports sabre's Backend\MockSharing (with notification
// support so the sharee round-trip can be exercised).
type MockSharingBackend struct {
	MockBackend
	MemoryNotificationsBackend
	Shares map[CalendarID][]Sharee
}

func (m *MockSharingBackend) UpdateInvites(id CalendarID, sharees []Sharee) error {
	if m.Shares == nil {
		m.Shares = map[CalendarID][]Sharee{}
	}
	for _, sharee := range sharees {
		existing := -1
		for i, cur := range m.Shares[id] {
			if cur.Href == sharee.Href {
				existing = i
			}
		}
		sharee.InviteStatus = InviteNoResponse
		switch {
		case sharee.Access == AccessNoAccess && existing >= 0:
			m.Shares[id] = append(m.Shares[id][:existing], m.Shares[id][existing+1:]...)
		case existing >= 0:
			m.Shares[id][existing] = sharee
		case sharee.Access != AccessNoAccess:
			m.Shares[id] = append(m.Shares[id], sharee)
		}
	}
	return nil
}

func (m *MockSharingBackend) Invites(id CalendarID) ([]Sharee, error) {
	return m.Shares[id], nil
}

func (m *MockSharingBackend) SetPublishStatus(id CalendarID, value bool) error { return nil }

// ShareReply mirrors sabre's mock: accepting yields a fixed calendar path.
func (m *MockSharingBackend) ShareReply(href string, status int, calendarURI, inReplyTo, summary string) (string, error) {
	if status == InviteAccepted {
		return "calendars/blabla/calendar", nil
	}
	return "", nil
}

// SharingPlugin handles the calendarserver sharing POSTs. Port of
// CalDAV\SharingPlugin + the relevant DAV\Sharing\Plugin behavior.
type SharingPlugin struct {
	server *dav.Server
}

func (p *SharingPlugin) PluginName() string { return "sharing" }

func (p *SharingPlugin) Init(s *dav.Server) { p.server = s }

func (p *SharingPlugin) Features() []string {
	return []string{"calendarserver-sharing"}
}

func (p *SharingPlugin) Methods(path string) []string { return []string{"POST"} }

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
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return false, nil
	}
	body, err := readBody(r)
	if err != nil {
		return true, err
	}
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
		sharees[i].Principal = p.principalForAddress(sharees[i].Href)
	}
	if err := sharing.UpdateInvites(calNode.Info.ID, sharees); err != nil {
		return err
	}
	// Drop an invite-notification into each added local sharee's inbox, so
	// their client's notification feed surfaces the share (like sabre).
	if notif, ok := calNode.backend.(NotificationsBackend); ok {
		for _, sharee := range sharees {
			if sharee.Access == AccessNoAccess || sharee.Principal == "" {
				continue
			}
			_ = notif.AddNotification(sharee.Principal, InviteNotification{
				ID:           notificationID(calPath, sharee.Href),
				DTStamp:      requestTime(r),
				HostURL:      calPath,
				Organizer:    calNode.Owner(),
				Access:       sharee.Access,
				InviteStatus: InviteNoResponse,
				Summary:      sharee.Comment,
			})
		}
	}
	w.Header().Set("X-Sabre-Status", "everything-went-well")
	w.WriteHeader(http.StatusOK)
	return nil
}

// requestTime returns a deterministic timestamp when available (parity/tests
// pin it), else now.
func requestTime(r *http.Request) time.Time {
	if v := r.Header.Get("X-Test-Now"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
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
	for _, plugin := range p.server.Plugins() {
		if checker, ok := plugin.(PrivilegeChecker); ok {
			if err := checker.CheckPrivileges(r.Context(), path, priv); err != nil {
				return err
			}
		}
	}
	return nil
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
	url, err := replies.ShareReply(reply.href, reply.status, reply.calendarURI, reply.inReplyTo, reply.summary)
	if err != nil {
		return err
	}
	w.Header().Set("X-Sabre-Status", "everything-went-well")
	if url != "" {
		// sabre resolves the returned href relative to the request URL
		// (LocalHref with contextUri = request path), so a home at
		// /calendars/user1 yields /calendars/<returned-path>.
		resolved := resolveHref(r.URL.Path, url)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte(xml.Header +
			`<cs:shared-as xmlns:d="DAV:" xmlns:cs="` + davxml.NSCalendarServer + `">` +
			`<d:href>` + xmlEscapeText(resolved) + `</d:href></cs:shared-as>`))
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
	if err := publisher.SetPublishStatus(calNode.Info.ID, value); err != nil {
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

// principalForAddress finds a principal by calendar user address (shared
// with the scheduling plugin's logic).
func (p *SharingPlugin) principalForAddress(address string) string {
	principals, err := p.server.Tree.NodeForPath("principals")
	if err != nil {
		return ""
	}
	coll, ok := principals.(dav.Collection)
	if !ok {
		return ""
	}
	children, err := coll.Children()
	if err != nil {
		return ""
	}
	for _, child := range children {
		ph, isPrincipal := child.(acl.PrincipalHolder)
		if !isPrincipal {
			continue
		}
		for _, candidate := range ph.AlternateURISet() {
			if strings.EqualFold(candidate, address) {
				return ph.PrincipalURL()
			}
		}
	}
	return ""
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

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
