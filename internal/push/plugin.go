package push

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"

	"calserver/internal/caldav"
	"calserver/internal/carddav"
	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// subscriptionsPrefix is the base-relative path space subscription resources
// live in; a DELETE here unsubscribes.
const subscriptionsPrefix = "push/subscriptions/"

// subscriptionTTL is how long a registration lives before the client must
// re-register. Comfortably above the spec's 3-day floor.
const subscriptionTTL = 7 * 24 * time.Hour

// Plugin adds WebDAV-Push to the DAV server: it advertises support
// (FeatureProvider + PropFindHook), accepts registrations (MethodProvider on
// POST), and handles unsubscribe (BeforeMethodHook on DELETE). Delivery is the
// worker's job, not the plugin's.
//
// Registration order matters (see PUSH-PLAN.md §5.4/§5.5): the plugin is added
// right after caldav.Plugin — after the ACL plugin, so its PropFind values
// respect the ACL read-deny latch; before caldav.SharingPlugin, so it sees a
// collection POST first and can pass a non-push body on intact.
type Plugin struct {
	cfg     Config
	store   *Store
	server  *dav.Server
	checker caldav.PrivilegeChecker
}

// NewPlugin builds the push plugin over the shared database pool.
func NewPlugin(cfg Config, store *Store) *Plugin {
	return &Plugin{cfg: cfg, store: store}
}

func (p *Plugin) PluginName() string { return "webdav-push" }

func (p *Plugin) Init(s *dav.Server) { p.server = s }

// Features implements dav.FeatureProvider: the "webdav-push" token in the DAV
// header (and OPTIONS) tells clients the server speaks WebDAV-Push.
func (p *Plugin) Features() []string { return []string{"webdav-push"} }

// Methods advertises POST (registration) in the Allow header.
func (p *Plugin) Methods(path string) []string { return []string{"POST"} }

// collectionKey type-switches a node to its (kind, numeric id); ok is false
// for any node that is not a push-capable collection.
func collectionKey(node dav.Node) (kind, id string, ok bool) {
	switch n := node.(type) {
	case *caldav.Calendar:
		return "calendar", n.Info.ID.Calendar, true
	case *carddav.AddressBook:
		return "addressbook", n.Info.ID, true
	}
	return "", "", false
}

// PropFind advertises transports, topic and supported-triggers on calendar and
// address-book collections. It runs after the ACL plugin, so on a node the
// principal cannot read pf.Handle is already a no-op (the read-deny latch),
// and the opaque topic never leaks.
func (p *Plugin) PropFind(pf *dav.PropFind, node dav.Node) error {
	kind, id, ok := collectionKey(node)
	if !ok {
		return nil
	}
	name := func(local string) xml.Name { return xml.Name{Space: NS, Local: local} }

	// transports: the Web Push transport plus our VAPID public key. The inner
	// fragment carries its own default xmlns so it is self-contained (the
	// marshaller declares NS on the multistatus root for the property name but
	// does not rewrite Raw content).
	pf.Handle(name("transports"), func() any {
		return davxml.Raw(`<web-push xmlns="` + NS + `">` +
			`<vapid-public-key type="p256ecdsa">` + escapeText(p.cfg.VAPIDPublicKey) + `</vapid-public-key>` +
			`</web-push>`)
	})
	pf.Handle(name("topic"), func() any { return p.cfg.Topic(kind, id) })
	pf.Handle(name("supported-triggers"), func() any {
		return davxml.Raw(`<content-update xmlns="` + NS + `">` +
			`<depth xmlns="DAV:">1</depth></content-update>`)
	})
	return nil
}

// HandleMethod claims a push-register POST. Any other POST body is handed back
// to the dispatch chain intact (caldav Add-Member, the scheduling outbox, the
// sharing actions), so this plugin is transparent to them.
func (p *Plugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != "POST" {
		return false, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return true, err
	}
	r.Body.Close()
	name, nameErr := rootElementName(body)
	if nameErr != nil || name != (xml.Name{Space: NS, Local: "push-register"}) {
		// Not ours: restore the body so the next MethodProvider (which reads
		// r.Body itself) still sees an intact stream, then pass through.
		r.Body = io.NopCloser(bytes.NewReader(body))
		return false, nil
	}
	return true, p.register(w, r, body)
}

// register stores (or refreshes) a subscription for the collection the POST
// targets and answers 201 with the subscription's Location and expiry.
func (p *Plugin) register(w http.ResponseWriter, r *http.Request, body []byte) error {
	path, err := p.server.Path(r)
	if err != nil {
		return err
	}
	node, err := p.server.Tree.NodeForPath(path)
	if err != nil {
		return err
	}
	kind, id, ok := collectionKey(node)
	if !ok {
		return dav.ErrForbidden("WebDAV-Push registration is only supported on calendar and address-book collections")
	}
	// A subscriber must be able to read the collection it wants notifications
	// for. POST is outside the ACL plugin's BeforeMethod table, so enforce it
	// here (like caldav's Add-Member enforces {DAV:}bind).
	checker, err := p.privilegeChecker()
	if err != nil {
		return err
	}
	if err := checker.CheckPrivileges(r.Context(), path, "{DAV:}read"); err != nil {
		return err
	}

	req, err := parseRegister(body)
	if err != nil {
		return dav.ErrBadRequest("%s", err)
	}
	current, _, err := p.store.CurrentToken(kind, id)
	if err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(subscriptionTTL)
	stored, err := p.store.Upsert(Subscription{
		Token:          token,
		Kind:           kind,
		CollectionID:   id,
		CollectionPath: path,
		PrincipalURI:   dav.Principal(r.Context()),
		PushResource:   req.pushResource,
		P256dh:         req.p256dh,
		AuthSecret:     req.authSecret,
	}, current, expires)
	if err != nil {
		return err
	}

	h := w.Header()
	h.Set("Location", p.server.BaseURI+subscriptionsPrefix+stored)
	h.Set("Expires", expires.UTC().Format(http.TimeFormat))
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// BeforeMethod intercepts DELETE of a subscription resource. It runs before
// CorePlugin's DELETE (which would 404 on this non-tree path) and after the
// ACL plugin (which passes a DELETE on a path with no tree node). Non-push
// DELETEs proceed untouched.
func (p *Plugin) BeforeMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != "DELETE" {
		return true, nil
	}
	path, err := p.server.Path(r)
	if err != nil || !strings.HasPrefix(path, subscriptionsPrefix) {
		return true, nil
	}
	token := strings.TrimPrefix(path, subscriptionsPrefix)
	deleted, err := p.store.DeleteByToken(token, dav.Principal(r.Context()))
	if err != nil {
		return false, err
	}
	if !deleted {
		return false, dav.ErrNotFound("no such push subscription")
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
	return false, nil
}

// privilegeChecker resolves (and caches) the ACL enforcement surface, failing
// closed when none is registered — the same fail-closed contract caldav's
// self-enforcing paths use.
func (p *Plugin) privilegeChecker() (caldav.PrivilegeChecker, error) {
	if p.checker != nil {
		return p.checker, nil
	}
	checker, err := caldav.RequirePrivilegeChecker(p.server)
	if err != nil {
		return nil, err
	}
	p.checker = checker
	return checker, nil
}

// randomToken returns a URL-safe opaque id for a subscription resource.
func randomToken() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
