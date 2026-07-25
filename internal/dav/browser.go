package dav

import (
	"html"
	"net/http"
	"sort"
	"strings"
)

// BrowserPlugin answers GET on collections with a minimal HTML index, like a
// stripped-down sabre Browser plugin (listing only — no POST actions, no
// asset serving). Without it a GET on a collection is a 501, which is
// sabre's default and what the corpus pins, so the plugin is opt-in
// (-browser flag); CalDAVTester start blocks GET calendar collections and
// need the 200.
type BrowserPlugin struct {
	server *Server
}

func (b *BrowserPlugin) PluginName() string { return "browser" }

func (b *BrowserPlugin) Init(s *Server) { b.server = s }

// HandleGet implements GetHandlerHook: serve an index for any collection.
// Registered last so content-type transforms and virtual-node handlers win.
func (b *BrowserPlugin) HandleGet(node Node, w http.ResponseWriter, r *http.Request) (bool, error) {
	coll, isColl := node.(Collection)
	if !isColl {
		return false, nil
	}

	children, err := coll.Children()
	if err != nil {
		return true, err
	}
	names := make([]string, 0, len(children))
	for _, child := range children {
		name := child.Name()
		if _, isChildColl := child.(Collection); isChildColl {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)

	base := strings.TrimSuffix(r.URL.Path, "/")
	var sb strings.Builder
	sb.WriteString("<html>\n<head><title>Index of " + html.EscapeString(r.URL.Path) + "</title></head>\n<body>\n")
	sb.WriteString("<h1>Index of " + html.EscapeString(r.URL.Path) + "</h1>\n<ul>\n")
	for _, name := range names {
		href := base + "/" + name
		sb.WriteString("<li><a href=\"" + html.EscapeString(href) + "\">" + html.EscapeString(name) + "</a></li>\n")
	}
	sb.WriteString("</ul>\n</body>\n</html>\n")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Collection listings are dynamic: no ETag, not cacheable.
	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte(sb.String()))
	return true, err
}
