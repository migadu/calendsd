package storage_test

// The domain-scoped principal listing (docs/slow-queries.md, item 2): the
// SQL domain rule agrees with acl.EmailDomain on every address shape, the
// query is served by its index, and a Depth:1 PROPFIND on /calendars/ no
// longer scans every principal nor looks each one up.

import (
	"encoding/xml"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"calendsd/internal/acl"
	"calendsd/internal/server"
	"calendsd/internal/storage"
	"calendsd/internal/storage/storagetest"
)

const domainCorpus = `
	INSERT INTO principals (uri, email, displayname) VALUES
		('principals/user1', 'user1@a.example', 'U1'),
		('principals/user2', 'user2@a.example', 'U2'),
		('principals/upper', 'upper@A.EXAMPLE', 'Upper'),
		('principals/twoat', 'two@at@a.example', 'Two'),
		('principals/trailing', 'trailing@', 'Trailing'),
		('principals/user3', 'user3@b.example', 'U3'),
		('principals/noemail', NULL, 'No'),
		('principals/user1/calendar-proxy-read', 'user1@a.example', NULL),
		('other/stranger', 'stranger@a.example', 'Elsewhere');`

func TestPrincipalsByPrefixInDomainMatchesGoRule(t *testing.T) {
	db := storagetest.NewDB(t, "calserver_b_principals_domain")
	mustExec(t, db, domainCorpus)
	principals := &storage.Principals{DB: db}

	all, err := principals.PrincipalsByPrefix(t.Context(), "principals")
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"a.example", "b.example", "example.org", "A.EXAMPLE"} {
		var want []string
		for _, p := range all {
			email, _ := p.Props[xml.Name{Space: "http://sabredav.org/ns", Local: "email-address"}].(string)
			if acl.EmailDomain(email) == strings.ToLower(domain) {
				want = append(want, p.URI)
			}
		}
		got, err := principals.PrincipalsByPrefixInDomain(t.Context(), "principals", domain)
		if err != nil {
			t.Fatal(err)
		}
		var gotURIs []string
		for _, p := range got {
			gotURIs = append(gotURIs, p.URI)
		}
		slices.Sort(want)
		slices.Sort(gotURIs)
		if !slices.Equal(gotURIs, want) {
			t.Errorf("domain %q: SQL %v, Go rule %v", domain, gotURIs, want)
		}
	}
}

func TestPrincipalsByPrefixInDomainUsesIndex(t *testing.T) {
	db := storagetest.NewDB(t, "calserver_b_principals_domain_index")
	mustExec(t, db, domainCorpus)
	// The table is tiny, so the planner would pick a scan on cost alone; with
	// scans discouraged, the plan shows whether the index can serve the query.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mustExec(t, db, `ANALYZE principals`)
	if _, err := tx.Exec(`SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(`EXPLAIN SELECT uri, email, displayname FROM principals
 WHERE uri LIKE $1 ESCAPE '\' AND uri NOT LIKE $2 ESCAPE '\'
   AND email LIKE '%@_%' AND lower(split_part(email, '@', -1)) = $3`,
		"principals/%", "principals/%/%", "a.example")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "principals_email_domain") {
		t.Errorf("plan does not use principals_email_domain:\n%s", strings.Join(plan, "\n"))
	}
}

// A Depth:1 PROPFIND on /calendars/ with 1000 principals across three
// domains reads principals with a couple of queries, not a scan plus one
// lookup per principal, and lists exactly the requester's domain.
func TestCalendarRootListingIsScopedInSQL(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_calendar_root_scoped")
	mustExec(t, db, `
		INSERT INTO principals (uri, email, displayname)
		SELECT 'principals/u' || i, 'u' || i || '@' || (ARRAY['a.example','b.example','c.example'])[i % 3 + 1], 'U' || i
		  FROM generate_series(1, 1000) AS i;`)
	srv := server.NewLoggedIn(db, server.Config{BaseURI: "/"}, "principals/u3") // u3 is in a.example

	qlog.Reset()
	req := httptest.NewRequest("PROPFIND", "/calendars/", strings.NewReader(
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`))
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 207 {
		t.Fatalf("PROPFIND = %d:\n%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	homes := strings.Count(body, "<d:href>/calendars/u")
	if homes != 333 { // u3, u6, ..., u999: i%3 == 0 → a.example
		t.Errorf("listed %d homes, want 333 (one domain)", homes)
	}
	if strings.Contains(body, "/calendars/u1/") || strings.Contains(body, "/calendars/u2/") {
		t.Errorf("listing leaked other domains:\n%s", body[:min(len(body), 800)])
	}

	var principalQueries int
	for _, s := range qlog.Statements() {
		if strings.Contains(s, "FROM principals") {
			principalQueries++
		}
	}
	if principalQueries > 3 {
		t.Errorf("listing ran %d principals queries, want at most 3: %q", principalQueries, qlog.Statements())
	}
	if total := qlog.Count(); total > homes+10 {
		t.Errorf("listing ran %d queries for %d homes, want about one per home: %q", total, homes, qlog.Statements())
	}
}
