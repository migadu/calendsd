package dav

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	davxml "calserver/internal/dav/xml"
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
}

// NewServer creates a server for the given root collection with the core
// WebDAV plugin registered.
func NewServer(baseURI string, root Collection) *Server {
	s := &Server{
		BaseURI:             normalizeBaseURI(baseURI),
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

func normalizeBaseURI(uri string) string {
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
	for _, p := range s.plugins {
		if fp, ok := p.(FeatureProvider); ok {
			for _, f := range fp.Features() {
				if !slices.Contains(out, f) {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

// AllowedMethods returns all HTTP methods plugins can handle for path,
// deduplicated, in registration order.
func (s *Server) AllowedMethods(path string) []string {
	var out []string
	for _, p := range s.plugins {
		if mp, ok := p.(MethodProvider); ok {
			for _, m := range mp.Methods(path) {
				if !slices.Contains(out, m) {
					out = append(out, m)
				}
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
	if proceed, err := s.checkPreconditions(w, r); err != nil {
		s.WriteError(w, err)
		return
	} else if !proceed {
		return
	}
	for _, p := range s.plugins {
		mp, ok := p.(MethodProvider)
		if !ok {
			continue
		}
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
		// Mirrors sabre's MethodNotAllowed::getHTTPHeaders.
		h.Set("Allow", strings.Join(s.AllowedMethods(""), ", "))
	}
	w.WriteHeader(de.Status)
	fmt.Fprintf(w, xml.Header+
		"<d:error xmlns:d=\"DAV:\" xmlns:s=\"%s\">\n"+
		"  <s:exception>%s</s:exception>\n"+
		"  <s:message>%s</s:message>\n"+
		"</d:error>\n",
		davxml.NSSabreDAV, xmlEscape(de.Exception), xmlEscape(de.Message))
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// PropertiesForPath runs the PROPFIND machinery for a path: depth 0 or 1,
// one multistatus response element per visited node. Port of sabre's
// getPropertiesIteratorForPath.
func (s *Server) PropertiesForPath(path string, names []xml.Name, depth int) ([]davxml.Response, error) {
	if depth != 0 {
		depth = 1 // propfind depth infinity stays disabled, like sabre's default
	}
	path = strings.Trim(path, "/")

	node, err := s.Tree.NodeForPath(path)
	if err != nil {
		return nil, err
	}

	type reqItem struct {
		pf   *PropFind
		node Node
	}
	pf := NewPropFind(path, names, depth)
	items := []reqItem{{pf, node}}
	if depth == 1 {
		if coll, isColl := node.(Collection); isColl {
			children, err := coll.Children()
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
		if err := s.runPropFind(item.pf, item.node); err != nil {
			return nil, err
		}
		href := s.BaseURI + encodePath(item.pf.Path())
		if isCollectionNode(item.node) && !strings.HasSuffix(href, "/") {
			href += "/"
		}
		out = append(out, item.pf.response(href))
	}
	return out, nil
}

// runPropFind fires the property hooks for one node in sabre's order:
// core defaults, plugins, then node-supplied dead properties.
func (s *Server) runPropFind(pf *PropFind, node Node) error {
	corePropFind(s, pf, node)
	for _, p := range s.plugins {
		if hook, ok := p.(PropFindHook); ok {
			if err := hook.PropFind(pf, node); err != nil {
				return err
			}
		}
	}
	// Late phase: node dead properties (sabre's propFindNode).
	if props, ok := node.(PropNode); ok {
		var want []xml.Name
		if !pf.IsAllProps() {
			want = pf.Props404()
			if len(want) == 0 {
				return nil
			}
		}
		values, err := props.Props(want)
		if err != nil {
			return err
		}
		for n, v := range values {
			pf.Set(n, v, http.StatusOK)
		}
	}
	return nil
}

// UpdateProperties runs the PROPPATCH machinery for a path. Port of sabre's
// updateProperties: protected-property rejection, then node handlers.
func (s *Server) UpdateProperties(path string, update *davxml.PropertyUpdate) (map[xml.Name]int, error) {
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

	node, err := s.Tree.NodeForPath(path)
	if err != nil {
		return nil, err
	}
	if props, ok := node.(PropNode); ok {
		props.PropPatch(pp)
	}
	result, _ := pp.Commit()
	return result, nil
}

func isCollectionNode(n Node) bool {
	_, ok := n.(Collection)
	return ok
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
