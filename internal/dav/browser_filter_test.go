package dav

// Audit 5.3: the browser index listed Children() directly, so the child
// filter the ACL plugin applies to PROPFIND (which hides other domains'
// principals) did not apply to it.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

type hidingFilter struct{ hide string }

func (hidingFilter) PluginName() string { return "hiding-filter" }
func (hidingFilter) Init(*Server)       {}
func (f hidingFilter) FilterChildren(_ context.Context, _ string, children []Node) []Node {
	var out []Node
	for _, c := range children {
		if c.Name() != f.hide {
			out = append(out, c)
		}
	}
	return out
}

func TestBrowserIndexAppliesChildFilters(t *testing.T) {
	root := newMemColl("root", newMemColl("visible"), newMemColl("hidden"))
	s := NewServer("/", root)
	s.AddPlugin(hidingFilter{hide: "hidden"})
	s.AddPlugin(&BrowserPlugin{})

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET / = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "visible/") {
		t.Errorf("index lacks the visible child:\n%s", body)
	}
	if strings.Contains(body, "hidden") {
		t.Errorf("index lists a child the filter hid:\n%s", body)
	}
}
