package dav

import (
	"net/http"
	"net/url"
	"strings"
)

// CorePlugin implements the baseline WebDAV protocol behavior, mirroring
// sabre's CorePlugin. Method implementations land incrementally; OPTIONS is
// the phase-0 deliverable.
type CorePlugin struct {
	server *Server
}

func (c *CorePlugin) PluginName() string { return "core" }

func (c *CorePlugin) Init(s *Server) { c.server = s }

// Features implements FeatureProvider. "1" and "3" are WebDAV compliance
// classes (RFC 4918); extended-mkcol is RFC 5689. Class "2" (locks) is
// deliberately absent — locks are out of MVP scope.
func (c *CorePlugin) Features() []string {
	return []string{"1", "3", "extended-mkcol"}
}

// Methods implements MethodProvider. Matches sabre CorePlugin's method set;
// entries are advertised in Allow even before their handlers are ported.
func (c *CorePlugin) Methods(path string) []string {
	return []string{
		"OPTIONS", "GET", "HEAD", "DELETE", "PROPFIND", "MKCOL",
		"PUT", "PROPPATCH", "COPY", "MOVE",
	}
}

func (c *CorePlugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	switch r.Method {
	case "OPTIONS":
		return true, c.options(w, r)
	}
	return false, nil
}

// options answers OPTIONS with the DAV and Allow headers, mirroring
// sabre's httpOptions.
func (c *CorePlugin) options(w http.ResponseWriter, r *http.Request) error {
	h := w.Header()
	h.Set("DAV", strings.Join(c.server.Features(), ", "))
	h.Set("Allow", strings.Join(c.server.AllowedMethods(r.URL.Path), ", "))
	h.Set("MS-Author-Via", "DAV")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
	return nil
}

// unescapePath decodes percent-encoding in a URL path, matching sabre's
// decodePath (which decodes each segment but keeps "/" literal).
func unescapePath(p string) (string, error) {
	if !strings.Contains(p, "%") {
		return p, nil
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return "", err
		}
		segs[i] = dec
	}
	return strings.Join(segs, "/"), nil
}
