package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calserver/internal/config"
)

func TestOptionsFromConfig(t *testing.T) {
	var cfg config.Config
	cfg.Server.Addr = ":8080"
	cfg.Server.APIAddr = "127.0.0.1:8081"
	cfg.Server.MetricsAddr = ":9464"
	cfg.Server.BaseURI = "dav"
	cfg.Server.Rcptd.URL = "http://127.0.0.1:8090/"
	o := OptionsFromConfig(cfg)
	for got, want := range map[string]string{
		o.ServerURL:  "http://127.0.0.1:8080/dav",
		o.APIURL:     "http://127.0.0.1:8081",
		o.MetricsURL: "http://127.0.0.1:9464/metrics",
		o.RcptdURL:   "http://127.0.0.1:8090",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if o := OptionsFromConfig(config.Config{}); o.APIURL != "" || o.MetricsURL != "" || o.RcptdURL != "" {
		t.Errorf("unconfigured listeners must be skipped, got %+v", o)
	}
}

const sampleMetrics = `# HELP go_sql_idle_connections The number of idle connections.
go_sql_idle_connections{db_name="calserver"} 4
go_sql_in_use_connections{db_name="calserver"} 2
go_sql_max_open_connections{db_name="calserver"} 20
go_sql_open_connections{db_name="calserver"} 6
go_sql_wait_count_total{db_name="calserver"} 1470
go_sql_wait_duration_seconds_total{db_name="calserver"} 0.668253322
go_sql_in_use_connections{db_name="other"} 99
`

func TestParsePoolStats(t *testing.T) {
	s, err := parsePoolStats(strings.NewReader(sampleMetrics))
	if err != nil {
		t.Fatal(err)
	}
	if !s.seen || s.inUse != 2 || s.maxOpen != 20 || s.idle != 4 || s.waits != 1470 || s.waitSeconds < 0.66 {
		t.Errorf("stats = %+v", s)
	}
}

func metricsServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/metrics"
}

func TestCheckPool(t *testing.T) {
	o := Options{Timeout: time.Second, HTTP: &http.Client{}}
	o.MetricsURL = metricsServer(t, sampleMetrics)
	if r := checkPool(context.Background(), o); r.Status != Healthy || !strings.Contains(r.Message, "2/20 in use") {
		t.Errorf("normal pool: %+v", r)
	}
	o.MetricsURL = metricsServer(t, strings.Replace(sampleMetrics, `in_use_connections{db_name="calserver"} 2`, `in_use_connections{db_name="calserver"} 20`, 1))
	if r := checkPool(context.Background(), o); r.Status != Degraded || !strings.HasPrefix(r.Message, "exhausted") {
		t.Errorf("pinned pool: %+v", r)
	}
	o.MetricsURL = "http://" + deadAddr(t) + "/metrics"
	if r := checkPool(context.Background(), o); r.Status != Unhealthy {
		t.Errorf("unreachable metrics: %+v", r)
	}
}

func TestCheckContactsAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	o := Options{APIURL: srv.URL, Timeout: time.Second, HTTP: &http.Client{}}
	if r := checkContactsAPI(context.Background(), o); r.Status != Healthy {
		t.Errorf("401 listener: %+v", r)
	}
	o.APIURL = "http://" + deadAddr(t)
	if r := checkContactsAPI(context.Background(), o); r.Status != Unhealthy {
		t.Errorf("closed port: %+v", r)
	}
}

func TestDSNNodes(t *testing.T) {
	nodes, err := dsnNodes("host=10.0.0.1,10.0.0.2,10.0.0.1 port=5433 sslmode=prefer user=u dbname=d")
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, n := range nodes {
		labels = append(labels, n.label)
		if n.cfg.Fallbacks != nil {
			t.Errorf("%s: a node config must not fall back to other hosts", n.label)
		}
	}
	if got := strings.Join(labels, " "); got != "10.0.0.1:5433 10.0.0.2:5433" {
		t.Errorf("nodes = %q, want each host once, in DSN order", got)
	}
}

// A node that accepts and never answers (dropped traffic behind a
// half-open path) and a node that refuses: both rows unhealthy, since no node
// answered, and the check ends at its timeout rather than the OS's.
func TestCheckNodesAllDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	stallHost, stallPort, _ := net.SplitHostPort(ln.Addr().String())
	_, deadPort, _ := net.SplitHostPort(deadAddr(t))

	o := Options{Timeout: 300 * time.Millisecond,
		DSN: "host=" + stallHost + "," + stallHost + " port=" + stallPort + "," + deadPort + " user=u dbname=d sslmode=disable"}
	start := time.Now()
	rows := checkNodes(context.Background(), o)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v; nodes must be probed concurrently under the timeout", took)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want one per node", rows)
	}
	for _, r := range rows {
		if r.Status != Unhealthy || !strings.HasPrefix(r.Component, "database[") {
			t.Errorf("row = %+v, want unhealthy database[host:port]", r)
		}
	}
	if o.DSN = "host=" + stallHost + " port=" + stallPort + " user=u dbname=d"; checkNodes(context.Background(), o) != nil {
		t.Error("a single-host DSN adds no node rows")
	}
}

func TestOneLine(t *testing.T) {
	in := "failed to connect to `user=u database=d`:\n\t10.0.0.1:5433 (10.0.0.1): dial error: refused\n\t10.0.0.2:5433 (10.0.0.2): dial error: i/o timeout"
	want := "10.0.0.1:5433 (10.0.0.1): dial error: refused 10.0.0.2:5433 (10.0.0.2): dial error: i/o timeout"
	if got := oneLine(in); got != want {
		t.Errorf("oneLine = %q, want %q", got, want)
	}
}

func TestFailedIgnoresDegraded(t *testing.T) {
	if Failed([]Result{{Status: Healthy}, {Status: Degraded}}) {
		t.Error("degraded must not fail the command")
	}
	if !Failed([]Result{{Status: Healthy}, {Status: Unhealthy}}) {
		t.Error("unhealthy must fail the command")
	}
}

// deadAddr is a loopback address with nothing listening.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}
