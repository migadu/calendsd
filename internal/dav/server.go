package dav

import (
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"
	"net/url"
	gopath "path"
	"slices"
	"strings"

	davxml "calserver/internal/dav/xml"
	"calserver/internal/metrics"
)

// Server is the WebDAV protocol engine: an http.Handler dispatching methods
// across an ordered plugin chain over a node tree. Port of sabre's
// DAV\Server.
type Server struct {
	// BaseURI is the URI prefix the server is mounted at, e.g. "/dav/".
	// Always begins and ends with "/".
	BaseURI string
	// Tree resolves paths to nodes.
	Tree *Tree

	plugins []Plugin

	// ProtectedProperties cannot be changed via PROPPATCH; mirrors
	// sabre's Server::$protectedProperties.
	ProtectedProperties []xml.Name

	// resourceTypeMappings mirror sabre's Server::$resourceTypeMapping:
	// plugins register predicates that add resourcetype values (e.g.
	// {DAV:}principal for principal nodes).
	resourceTypeMappings []func(Node) []xml.Name

	// MaxRequestBodySize caps how many bytes any request body may deliver;
	// larger bodies are rejected with 413. Guards against memory-exhaustion
	// from unbounded PUT/REPORT/PROPFIND bodies. Zero means the default.
	MaxRequestBodySize int64

	// Metrics, when non-nil, receives protocol-level observations (REPORT
	// types, sync truncation). A nil Set records nothing.
	Metrics *metrics.Set
}

// DefaultMaxRequestBodySize is the request-body cap applied when a Server does
// not set MaxRequestBodySize. 100 MiB comfortably covers real calendar/contact
// payloads while bounding per-request allocation.
const DefaultMaxRequestBodySize = 100 << 20

// AddResourceTypeMapping registers an additional resourcetype source.
func (s *Server) AddResourceTypeMapping(fn func(Node) []xml.Name) {
	s.resourceTypeMappings = append(s.resourceTypeMappings, fn)
}

// ResourceTypesForNode computes a node's {DAV:}resourcetype values,
// mirroring sabre's getResourceTypeForNode.
func (s *Server) ResourceTypesForNode(node Node) []xml.Name {
	var out []xml.Name
	if isCollectionNode(node) {
		out = append(out, xml.Name{Space: davxml.NSDav, Local: "collection"})
	}
	for _, fn := range s.resourceTypeMappings {
		out = append(out, fn(node)...)
	}
	return out
}

// NewServer creates a server for the given root collection with the core
// WebDAV plugin registered.
func NewServer(baseURI string, root Collection) *Server {
	s := &Server{
		BaseURI:             NormalizeBaseURI(baseURI),
		Tree:                NewTree(root),
		ProtectedProperties: defaultProtectedProperties(),
	}
	s.AddPlugin(&CorePlugin{})
	return s
}

func defaultProtectedProperties() []xml.Name {
	dav := func(local string) xml.Name { return xml.Name{Space: davxml.NSDav, Local: local} }
	return []xml.Name{
		// RFC 4918
		dav("getcontentlength"), dav("getetag"), dav("getlastmodified"),
		dav("lockdiscovery"), dav("supportedlock"),
		// RFC 4331
		dav("quota-available-bytes"), dav("quota-used-bytes"),
		// RFC 3744
		dav("supported-privilege-set"), dav("current-user-privilege-set"),
		dav("acl"), dav("acl-restrictions"), dav("inherited-acl-set"),
		// RFC 3253
		dav("supported-method-set"), dav("supported-report-set"),
		// RFC 6578
		dav("sync-token"),
		{Space: davxml.NSCalendarServer, Local: "ctag"},
		{Space: davxml.NSSabreDAV, Local: "sync-token"},
	}
}

// NormalizeBaseURI gives a base URI the leading and trailing slash the
// server mounts it with ("" is "/").
func NormalizeBaseURI(uri string) string {
	if uri == "" {
		return "/"
	}
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	return uri
}

// AddPlugin registers a plugin and initializes it.
func (s *Server) AddPlugin(p Plugin) {
	s.plugins = append(s.plugins, p)
	p.Init(s)
}

// Plugins returns all registered plugins in registration order.
func (s *Server) Plugins() []Plugin { return s.plugins }

// Plugin returns the registered plugin with the given name, or nil.
func (s *Server) Plugin(name string) Plugin {
	for _, p := range s.plugins {
		if p.PluginName() == name {
			return p
		}
	}
	return nil
}

// Features returns all DAV compliance tokens contributed by plugins,
// deduplicated, in registration order.
func (s *Server) Features() []string {
	var out []string
	for fp := range Hooks[FeatureProvider](s) {
		for _, f := range fp.Features() {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

// AllowedMethods returns all HTTP methods plugins can handle for path,
// deduplicated, in registration order.
func (s *Server) AllowedMethods(ctx context.Context, path string) []string {
	var out []string
	for mp := range Hooks[MethodProvider](s) {
		for _, m := range mp.Methods(ctx, path) {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// Path translates a request URI into a server-relative path with the BaseURI
// stripped, mirroring sabre's calculateUri: no leading or trailing slash.
func (s *Server) Path(r *http.Request) (string, error) {
	return s.stripBase(r.URL.EscapedPath())
}

// ResolveHref resolves an absolute-or-URL href (e.g. the Destination
// header) into a server-relative path.
func (s *Server) ResolveHref(href string) (string, error) {
	return s.stripBase(href)
}

// stripBase resolves an absolute-or-URL destination (e.g. the Destination
// header) into a server-relative path.
func (s *Server) stripBase(p string) (string, error) {
	if i := strings.Index(p, "://"); i >= 0 {
		u, err := url.Parse(p)
		if err != nil {
			return "", ErrBadRequest("invalid uri: %s", p)
		}
		p = u.EscapedPath()
	}
	// Normalize ./.. segments and duplicate slashes like sabre's
	// Uri\normalize before comparing against the base URI.
	if p != "" {
		trailing := strings.HasSuffix(p, "/")
		p = gopath.Clean("/" + strings.TrimPrefix(p, "/"))
		if trailing && p != "/" {
			p += "/"
		}
	}
	if p+"/" == s.BaseURI {
		// Request for the base itself without the trailing slash.
		return "", nil
	}
	if !strings.HasPrefix(p, s.BaseURI) {
		return "", ErrForbidden("Requested uri (%s) is out of base uri (%s)", p, s.BaseURI)
	}
	p = strings.TrimPrefix(p, s.BaseURI)
	p = strings.Trim(p, "/")
	return unescapePath(p)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	limit := s.MaxRequestBodySize
	if limit <= 0 {
		limit = DefaultMaxRequestBodySize
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	r = r.WithContext(withPrincipalState(r.Context()))
	// beforeMethod hooks (auth, then ACL) run before precondition checks,
	// matching sabre's event order. The order is load-bearing for privacy,
	// not just parity: because ACL rejects an unauthorized principal here
	// first, a conditional request never reaches checkPreconditions below,
	// so a 412/304 can't reveal a resource's existence or ETag to a client
	// that lacks privilege on it. Do not move preconditions ahead of ACL.
	for hook := range Hooks[BeforeMethodHook](s) {
		proceed, err := hook.BeforeMethod(w, r)
		if err != nil {
			s.WriteError(w, err)
			return
		}
		if !proceed {
			return
		}
	}
	if proceed, err := s.checkPreconditions(w, r); err != nil {
		s.WriteError(w, err)
		return
	} else if !proceed {
		return
	}
	for mp := range Hooks[MethodProvider](s) {
		handled, err := mp.HandleMethod(w, r)
		if err != nil {
			s.WriteError(w, err)
			return
		}
		if handled {
			return
		}
	}
	s.WriteError(w, ErrNotImplemented(
		"There was no plugin in the system that was willing to handle this %s method. Enable the Browser plugin to get a better result here.",
		r.Method))
}

// WriteError serializes an error the way sabre does: an XML {DAV:}error body
// carrying the exception identity.
func (s *Server) WriteError(w http.ResponseWriter, err error) {
	de := AsError(err)
	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	if de.Status == http.StatusMethodNotAllowed {
		// Mirrors sabre's MethodNotAllowed::getHTTPHeaders. The methods of
		// the in-memory root need no storage, so no request context.
		h.Set("Allow", strings.Join(s.AllowedMethods(context.Background(), ""), ", "))
	}
	w.WriteHeader(de.Status)
	body := errorBody{Exception: de.Exception, Message: de.Message}
	for _, name := range de.Precondition {
		body.Precondition = append(body.Precondition, emptyElement{XMLName: name})
	}
	out, err := xml.MarshalIndent(body, "", "  ")
	if err != nil {
		return
	}
	_, _ = w.Write(append([]byte(xml.Header), append(out, '\n')...))
}

// errorBody is the {DAV:}error document WriteError sends: the precondition
// elements, then the sabre exception identity.
type errorBody struct {
	XMLName      xml.Name `xml:"DAV: error"`
	Precondition []emptyElement
	Exception    string `xml:"http://sabredav.org/ns exception"`
	Message      string `xml:"http://sabredav.org/ns message"`
}

// emptyElement marshals as an empty element named by its XMLName.
type emptyElement struct {
	XMLName xml.Name
}

// WriteMultiStatus writes ms as a 207 Multi-Status response.
func (s *Server) WriteMultiStatus(w http.ResponseWriter, ms *davxml.MultiStatus) error {
	body, err := ms.Marshal()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(body)
	return err
}

// PropertiesForPath runs the PROPFIND machinery for a path: depth 0 or 1,
// one multistatus response element per visited node. Port of sabre's
// getPropertiesIteratorForPath.
func (s *Server) PropertiesForPath(ctx context.Context, path string, names []xml.Name, depth int, include ...xml.Name) ([]davxml.Response, error) {
	if depth != 0 {
		depth = 1 // propfind depth infinity stays disabled, like sabre's default
	}
	path = strings.Trim(path, "/")

	node, err := s.Tree.NodeForPath(ctx, path)
	if err != nil {
		return nil, err
	}

	type reqItem struct {
		pf   *PropFind
		node Node
	}
	pf := NewPropFind(ctx, path, names, depth)
	// <include> properties are requested alongside allprop without
	// suppressing allprop semantics (RFC 4918 §9.1).
	for _, n := range include {
		pf.Request(n)
	}
	items := []reqItem{{pf, node}}
	if depth == 1 {
		if coll, isColl := node.(Collection); isColl {
			children, err := s.VisibleChildren(ctx, path, coll)
			if err != nil {
				return nil, err
			}
			for _, child := range children {
				childPath := child.Name()
				if path != "" {
					childPath = path + "/" + child.Name()
				}
				items = append(items, reqItem{pf.clone(childPath), child})
			}
		}
	}

	var out []davxml.Response
	for _, item := range items {
		if err := s.runPropFind(ctx, item.pf, item.node); err != nil {
			return nil, err
		}
		// WebDAV recommends (and iCal demands, for principals) a
		// trailing slash on collection and principal hrefs.
		href := s.Href(item.pf.Path())
		types := s.ResourceTypesForNode(item.node)
		slash := slices.Contains(types, xml.Name{Space: davxml.NSDav, Local: "collection"}) ||
			slices.Contains(types, xml.Name{Space: davxml.NSDav, Local: "principal"})
		if slash && !strings.HasSuffix(href, "/") {
			href += "/"
		}
		out = append(out, item.pf.response(href))
	}
	return out, nil
}

// runPropFind fires the property hooks for one node in sabre's order:
// core defaults, plugins, then node-supplied dead properties.
func (s *Server) runPropFind(ctx context.Context, pf *PropFind, node Node) error {
	corePropFind(s, pf, node)
	for hook := range Hooks[PropFindHook](s) {
		if err := hook.PropFind(pf, node); err != nil {
			return err
		}
	}
	// Late phase: node dead properties (sabre's propFindNode at 120).
	if props, ok := node.(PropNode); ok {
		var want []xml.Name
		if !pf.IsAllProps() {
			want = pf.Props404()
		}
		if pf.IsAllProps() || len(want) > 0 {
			values, err := props.Props(ctx, want)
			if err != nil {
				return err
			}
			for n, v := range values {
				pf.Set(n, v, http.StatusOK)
			}
		}
	}
	// Very late phase: property storage (sabre's priority 130).
	for hook := range Hooks[LatePropFindHook](s) {
		if err := hook.PropFindLate(pf, node); err != nil {
			return err
		}
	}
	return nil
}

// UpdateProperties runs the PROPPATCH machinery for a path. Port of sabre's
// updateProperties: protected-property rejection, then node handlers.
func (s *Server) UpdateProperties(ctx context.Context, path string, update *davxml.PropertyUpdate) (map[xml.Name]int, error) {
	pp := NewPropPatch(update.Values)

	var protected []xml.Name
	for n := range update.Values {
		if slices.Contains(s.ProtectedProperties, n) {
			protected = append(protected, n)
		}
	}
	if len(protected) > 0 {
		pp.SetResultCode(protected, http.StatusForbidden)
	}

	node, err := s.Tree.NodeForPath(ctx, path)
	if err != nil {
		return nil, err
	}
	// Pre-node hooks may claim mutations first (per-user property storage).
	for hook := range Hooks[PrePropPatchHook](s) {
		if err := hook.PrePropPatch(ctx, path, node, pp); err != nil {
			return nil, err
		}
	}
	if props, ok := node.(PropNode); ok {
		props.PropPatch(ctx, pp)
	}
	// Property-storage plugins claim the remainder (sabre priority 300).
	for hook := range Hooks[PropPatchHook](s) {
		if err := hook.PropPatch(ctx, path, pp); err != nil {
			return nil, err
		}
	}
	result, _ := pp.Commit()
	return result, nil
}

// fireAfterUnbind notifies plugins that a node was removed.
func (s *Server) fireAfterUnbind(ctx context.Context, path string) {
	for hook := range Hooks[AfterUnbindHook](s) {
		// The node is already gone, so a failing listener cannot undo the
		// request; it can only leave state behind (e.g. dead properties).
		if err := hook.AfterUnbind(ctx, path); err != nil {
			slog.ErrorContext(ctx, "after-unbind hook failed", "plugin", hook.(Plugin).PluginName(), "path", path, "err", err)
		}
	}
}

// fireAfterMove notifies plugins that a node moved.
func (s *Server) fireAfterMove(ctx context.Context, source, destination string) {
	for hook := range Hooks[AfterMoveHook](s) {
		if err := hook.AfterMove(ctx, source, destination); err != nil {
			slog.ErrorContext(ctx, "after-move hook failed", "plugin", hook.(Plugin).PluginName(),
				"source", source, "destination", destination, "err", err)
		}
	}
}

func isCollectionNode(n Node) bool {
	_, ok := n.(Collection)
	return ok
}

// VisibleChildren lists a collection's children minus those a plugin hides
// from the requester (e.g. domain-scoped principal listings). Every listing
// goes through here, so a filtered child leaves no trace in any response.
func (s *Server) VisibleChildren(ctx context.Context, path string, coll Collection) ([]Node, error) {
	children, err := coll.Children(ctx)
	if err != nil {
		return nil, err
	}
	for hook := range Hooks[ChildFilterHook](s) {
		children = hook.FilterChildren(ctx, path, children)
	}
	return children, nil
}

// Href renders a server-relative path as an href: the base URI plus the
// percent-encoded path. ResolveHref is its inverse.
func (s *Server) Href(path string) string {
	return s.BaseURI + encodePath(path)
}

// encodePath percent-encodes a path, segment-preserving, like sabre's
// HTTP\encodePath.
func encodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}
