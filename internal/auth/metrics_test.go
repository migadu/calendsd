package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"calserver/internal/metrics"
)

func attemptsWant(lines ...string) string {
	return "# HELP calserver_auth_attempts_total Authentication attempts by backend and typed outcome.\n" +
		"# TYPE calserver_auth_attempts_total counter\n" +
		strings.Join(lines, "\n") + "\n"
}

func TestBasicBackendMetrics(t *testing.T) {
	set := metrics.New("test")
	b := &BasicBackend{Realm: "r", Metrics: set,
		Validate: func(u, p string) bool { return p == "good" }}

	ok := httptest.NewRequest("GET", "/", nil)
	ok.SetBasicAuth("hans", "good")
	bad := httptest.NewRequest("GET", "/", nil)
	bad.SetBasicAuth("hans", "bad")
	none := httptest.NewRequest("GET", "/", nil)

	check(b, ok)
	check(b, bad)
	check(b, none) // no Basic header: must not count

	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(attemptsWant(
		`calserver_auth_attempts_total{backend="basic",outcome="bad_credentials"} 1`,
		`calserver_auth_attempts_total{backend="basic",outcome="ok"} 1`,
	)), "calserver_auth_attempts_total"); err != nil {
		t.Error(err)
	}
	if n := testutil.CollectAndCount(set.Registry(), "calserver_auth_check_duration_seconds"); n != 1 {
		t.Errorf("auth_check_duration series = %d, want 1 (basic only)", n)
	}
}

func TestPluginCountsCredentiallessProbe(t *testing.T) {
	set := metrics.New("test")
	p := NewPlugin(&BasicBackend{Realm: "r", Metrics: set,
		Validate: func(u, pw string) bool { return false }})
	p.Metrics = set

	r := httptest.NewRequest("PROPFIND", "/", nil) // no credentials at all
	w := httptest.NewRecorder()
	if proceed, err := p.BeforeMethod(w, r); proceed || err == nil {
		t.Fatal("expected auth rejection")
	}

	// An Authorization header no backend can parse is still an attempt (a
	// brute-force tool sending Bearer must not read as a routine probe).
	bearer := httptest.NewRequest("PROPFIND", "/", nil)
	bearer.Header.Set("Authorization", "Bearer sometoken")
	if proceed, err := p.BeforeMethod(httptest.NewRecorder(), bearer); proceed || err == nil {
		t.Fatal("expected auth rejection for bearer header")
	}

	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(attemptsWant(
		`calserver_auth_attempts_total{backend="none",outcome="error"} 1`,
		`calserver_auth_attempts_total{backend="none",outcome="no_credentials"} 1`,
	)), "calserver_auth_attempts_total"); err != nil {
		t.Error(err)
	}
}

func TestRcptdBackendMetrics(t *testing.T) {
	hash := bcryptHashT(t, "good")
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("email") {
		case "hans@example.org":
			w.Write([]byte(`{"password_hashes":["` + hash + `"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fake.Close()

	set := metrics.New("test")
	b := NewRcptdBackend("r", fake.URL, "", 0) // ttl 0: cache disabled
	b.Metrics = set

	login := func(user, pass string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetBasicAuth(user, pass)
		check(b, r)
	}
	login("hans@example.org", "good")
	login("hans@example.org", "wrong")
	login("unknown@example.org", "x")

	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(attemptsWant(
		`calserver_auth_attempts_total{backend="rcptd",outcome="bad_credentials"} 2`,
		`calserver_auth_attempts_total{backend="rcptd",outcome="ok"} 1`,
	)), "calserver_auth_attempts_total"); err != nil {
		t.Error(err)
	}

	want := `
# HELP calserver_rcptd_requests_total HTTP lookups against rcptd /auth by status code (or "error" for transport failures).
# TYPE calserver_rcptd_requests_total counter
calserver_rcptd_requests_total{code="200"} 2
calserver_rcptd_requests_total{code="404"} 1
`
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(want),
		"calserver_rcptd_requests_total"); err != nil {
		t.Error(err)
	}
}
