package dav

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Server is the WebDAV protocol engine: an http.Handler dispatching methods
// across an ordered plugin chain. Port of sabre's DAV\Server.
type Server struct {
	// BaseURI is the URI prefix the server is mounted at, e.g. "/dav/".
	// Always begins and ends with "/".
	BaseURI string

	plugins []Plugin
}

// NewServer creates a server with the core WebDAV plugin registered.
func NewServer(baseURI string) *Server {
	s := &Server{BaseURI: normalizeBaseURI(baseURI)}
	s.AddPlugin(&CorePlugin{})
	return s
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
	p := r.URL.EscapedPath()
	if p+"/" == s.BaseURI {
		// Request for the base itself without the trailing slash.
		return "", nil
	}
	if !strings.HasPrefix(p, s.BaseURI) {
		return "", fmt.Errorf("requested uri %q is outside of base uri %q", p, s.BaseURI)
	}
	p = strings.TrimPrefix(p, s.BaseURI)
	p = strings.Trim(p, "/")
	return unescapePath(p)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, p := range s.plugins {
		mp, ok := p.(MethodProvider)
		if !ok {
			continue
		}
		handled, err := mp.HandleMethod(w, r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if handled {
			return
		}
	}
	// No plugin claimed the method.
	w.Header().Set("Allow", strings.Join(s.AllowedMethods(r.URL.Path), ", "))
	http.Error(w, "Method not implemented", http.StatusNotImplemented)
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	// Error-to-multistatus mapping grows with the port; keep it plain until
	// the XML layer lands.
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
