package dav

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// BrowserPlugin answers GET on collections with a minimal HTML index
// (listing only — no POST actions, no asset serving). Without it a GET on a collection is a 501, which is
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

	path, err := b.server.Path(r)
	if err != nil {
		return true, err
	}
	children, err := b.server.VisibleChildren(r.Context(), strings.Trim(path, "/"), coll)
	if err != nil {
		return true, err
	}
	type entry struct{ name, href string }
	entries := make([]entry, 0, len(children))
	for _, child := range children {
		name, href := child.Name(), url.PathEscape(child.Name())
		if _, isChildColl := child.(Collection); isChildColl {
			name += "/"
			href += "/"
		}
		entries = append(entries, entry{name, href})
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })

	base := strings.TrimSuffix(r.URL.EscapedPath(), "/")
	title := html.EscapeString(r.URL.Path)
	var sb strings.Builder
	fmt.Fprintf(&sb, "<html>\n<head><title>Index of %s</title></head>\n<body>\n", title)
	fmt.Fprintf(&sb, "<h1>Index of %s</h1>\n<ul>\n", title)
	for _, e := range entries {
		fmt.Fprintf(&sb, "<li><a href=\"%s\">%s</a></li>\n",
			html.EscapeString(base+"/"+e.href), html.EscapeString(e.name))
	}
	sb.WriteString("</ul>\n</body>\n</html>\n")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Collection listings are dynamic: no ETag, not cacheable.
	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte(sb.String()))
	return true, err
}
