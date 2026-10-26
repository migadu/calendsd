package admin

import (
	"net/http"
	"strings"
	"testing"
)

// The principals page lists a page at a time with next/previous cursors and
// filters by a search term (docs/slow-queries.md, item 5). testApp has
// alice, bob and ghost; with two per page that is two pages.
func TestPrincipalsPagePagesAndSearches(t *testing.T) {
	principalsPageSize = 2
	t.Cleanup(func() { principalsPageSize = 100 })
	h := testApp().Handler()

	body := get(t, h, "/principals").Body.String()
	for _, want := range []string{"principals/alice", "principals/bob", `after=principals%2fbob`} {
		if !strings.Contains(body, want) {
			t.Errorf("first page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "principals/ghost") || strings.Contains(body, "previous") {
		t.Errorf("first page shows the second page's principal or a previous link:\n%s", body)
	}

	body = get(t, h, "/principals?after=principals/bob").Body.String()
	if !strings.Contains(body, "principals/ghost") || !strings.Contains(body, `before=principals%2fghost`) {
		t.Errorf("second page lacks ghost or the previous cursor:\n%s", body)
	}
	if strings.Contains(body, "principals/alice") || strings.Contains(body, "next &rarr;") {
		t.Errorf("second page shows the first page's principal or a next link:\n%s", body)
	}

	body = get(t, h, "/principals?before=principals/ghost").Body.String()
	for _, want := range []string{"principals/alice", "principals/bob", `after=principals%2fbob`} {
		if !strings.Contains(body, want) {
			t.Errorf("page before ghost lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "previous") {
		t.Errorf("page before ghost is the first page and offers a previous link:\n%s", body)
	}

	body = get(t, h, "/principals?q=ali").Body.String()
	if !strings.Contains(body, "principals/alice") || strings.Contains(body, "principals/bob") || !strings.Contains(body, `value="ali"`) {
		t.Errorf("search for ali:\n%s", body)
	}

	data := getJSON[PrincipalsData](t, h, "/api/principals?q=example.org")
	if len(data.Principals) != 0 || data.Search != "example.org" {
		t.Errorf("search is a prefix on name or email, not a substring: %+v", data)
	}
	data = getJSON[PrincipalsData](t, h, "/api/principals")
	if len(data.Principals) != 2 || data.NextAfter != "principals/bob" || data.PrevBefore != "" {
		t.Errorf("first JSON page: %+v", data)
	}
	if w := get(t, h, "/principals?after=principals/zzz"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "no principals") {
		t.Errorf("page past the end = %d:\n%s", w.Code, w.Body.String())
	}
}
