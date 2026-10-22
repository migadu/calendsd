// Package health is the connectivity and liveness report behind
// `calctl health`: one row per component the running calserver depends on,
// probed the way calserver itself reaches it and graded healthy, degraded or
// unhealthy. The row shape and the table follow mqctl/rcptctl in the sibling
// services, so the same eyes and the same --json consumers read all three.
//
// Every check runs concurrently under its own timeout, so one stalled
// dependency costs one timeout, not the sum of them — the report has to stay
// usable precisely when something hangs.
package health

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"calserver/internal/config"
	"calserver/internal/pgdial"
	"calserver/internal/storage"
)

// DefaultTimeout bounds each check.
const DefaultTimeout = 5 * time.Second

// Status values. Only Unhealthy fails the command; Degraded is a warning.
const (
	Healthy   = "healthy"
	Degraded  = "degraded"
	Unhealthy = "unhealthy"
)

// Result is one row of the report.
type Result struct {
	Component string `json:"component"`
	Status    string `json:"status"`
	Endpoint  string `json:"endpoint,omitempty"`
	Message   string `json:"message,omitempty"`
	Duration  string `json:"duration"`
}

// Options says what to probe. Empty URLs skip their component, except
// ServerURL: the DAV listener is the service, so it is always reported.
type Options struct {
	ServerURL  string // DAV listener (OPTIONS probe)
	APIURL     string // webmail contacts API base, from [server] api_addr
	MetricsURL string // the daemon's /metrics, from [server] metrics_addr
	DSN        string // [database] dsn; empty is reported as degraded
	RcptdURL   string // [server.rcptd] url
	RcptdToken string
	Timeout    time.Duration // per check; zero takes DefaultTimeout
	HTTP       *http.Client  // nil uses a client without its own timeout (ctx bounds it)
	// ServerName is the TLS name the DAV probe verifies when ServerURL is
	// https. The probe dials loopback, which the certificate does not name,
	// so it is taken from the certificate itself; empty uses the URL host.
	ServerName string
}

// OptionsFromConfig derives every probe target from the same config file the
// daemon reads, turning wildcard listen hosts into loopback.
func OptionsFromConfig(cfg config.Config) Options {
	o := Options{
		ServerURL:  ServerURL(cfg),
		DSN:        cfg.Database.DSN,
		RcptdURL:   strings.TrimRight(cfg.Server.Rcptd.URL, "/"),
		RcptdToken: cfg.Server.Rcptd.Token,
	}
	if cfg.Server.TLS.Cert != "" {
		o.ServerName = certName(cfg.Server.TLS.Cert)
	}
	if o.RcptdToken == "" {
		o.RcptdToken = os.Getenv("RCPTD_AUTH_TOKEN")
	}
	if cfg.Server.APIAddr != "" {
		o.APIURL = "http://" + loopback(cfg.Server.APIAddr, "")
	}
	if cfg.Server.MetricsAddr != "" {
		o.MetricsURL = "http://" + loopback(cfg.Server.MetricsAddr, "") + "/metrics"
	}
	return o
}

// ServerURL is the DAV probe URL: the listen address with a wildcard host made
// loopback, HTTPS when a certificate is configured, and the mount point.
func ServerURL(cfg config.Config) string {
	addr := cfg.Server.Addr
	if addr == "" {
		addr = ":8080"
	}
	scheme := "http"
	if cfg.Server.TLS.Cert != "" {
		scheme = "https"
	}
	base := cfg.Server.BaseURI
	if base == "" {
		base = "/"
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	return scheme + "://" + loopback(addr, "8080") + base
}

// certName is the first name the PEM certificate at path is valid for, or ""
// when it cannot be read (then the probe reports the TLS error it gets).
func certName(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return ""
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return ""
		}
		if len(leaf.DNSNames) > 0 {
			return leaf.DNSNames[0]
		}
		return leaf.Subject.CommonName
	}
}

// loopback rewrites a listen address into one a local client can dial.
func loopback(addr, defaultPort string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, defaultPort
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// Run performs every configured check and returns the rows in a fixed order:
// calserver, contacts-api, database, database[node]…, db-pool, jobs, rcptd.
func Run(ctx context.Context, o Options) []Result {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}

	var (
		wg                       sync.WaitGroup
		server, api, pool, rcptd []Result
		db, nodes                []Result
	)
	spawn := func(dst *[]Result, fn func() []Result) {
		wg.Add(1)
		go func() { defer wg.Done(); *dst = fn() }()
	}
	spawn(&server, func() []Result { return []Result{checkServer(ctx, o)} })
	if o.APIURL != "" {
		spawn(&api, func() []Result { return []Result{checkContactsAPI(ctx, o)} })
	}
	// The database row and the jobs row share one pool; the per-node rows
	// dial each host on their own.
	spawn(&db, func() []Result { return checkDatabase(ctx, o) })
	spawn(&nodes, func() []Result { return checkNodes(ctx, o) })
	if o.MetricsURL != "" {
		spawn(&pool, func() []Result { return []Result{checkPool(ctx, o)} })
	}
	if o.RcptdURL != "" {
		spawn(&rcptd, func() []Result { return []Result{checkRcptd(ctx, o)} })
	}
	wg.Wait()

	out := append(server, api...)
	if len(db) > 0 {
		out = append(out, db[0])
	}
	out = append(out, nodes...)
	out = append(out, pool...)
	if len(db) > 1 {
		out = append(out, db[1:]...)
	}
	return append(out, rcptd...)
}

// Failed reports whether any row is unhealthy — the command's exit status.
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Status == Unhealthy {
			return true
		}
	}
	return false
}

// PrintTable renders the report as calctl/mqctl/rcptctl do, coloring the
// status only when w is a terminal so piped output stays plain.
func PrintTable(w io.Writer, results []Result) {
	printTable(w, results, isTerminal(w))
}

func printTable(w io.Writer, results []Result, color bool) {
	fmt.Fprintln(w, "Health Check")
	fmt.Fprintln(w, strings.Repeat("=", 70))

	// Columns are padded here rather than by text/tabwriter, which counts the
	// ANSI color codes as width and so shifts every colored row against the
	// plain header.
	rows := [][]string{{"COMPONENT", "STATUS", "ENDPOINT", "MESSAGE", "DURATION"}}
	for _, r := range results {
		endpoint := r.Endpoint
		if endpoint == "" {
			endpoint = "-"
		}
		rows = append(rows, []string{r.Component, r.Status, endpoint, r.Message, r.Duration})
	}
	// Widths count runes, as fmt's padding does: messages carry non-ASCII
	// (the em dash in rcptd's token error).
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	for n, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i == len(row)-1 {
				b.WriteString(cell)
				break
			}
			padded := fmt.Sprintf("%-*s  ", widths[i], cell)
			if i == 1 && n > 0 && color {
				padded = colorStatus(cell) + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2)
			}
			b.WriteString(padded)
		}
		fmt.Fprintln(w, b.String())
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func colorStatus(status string) string {
	switch status {
	case Healthy:
		return "\033[32m" + status + "\033[0m"
	case Unhealthy:
		return "\033[31m" + status + "\033[0m"
	case Degraded:
		return "\033[33m" + status + "\033[0m"
	default:
		return status
	}
}

// --- checks ------------------------------------------------------------------

// oneLine keeps a message to one table row: pgx reports a multi-host
// connect failure one host per line, and repeats the user/database prefix the
// row's component already implies.
func oneLine(msg string) string {
	if strings.HasPrefix(msg, "failed to connect to `") {
		if _, rest, ok := strings.Cut(msg, "`:"); ok {
			msg = rest
		}
	}
	return strings.Join(strings.Fields(msg), " ")
}

type timer time.Time

func startTimer() timer { return timer(time.Now()) }

func (t timer) result(component, status, endpoint, msg string) Result {
	return Result{Component: component, Status: status, Endpoint: endpoint, Message: oneLine(msg),
		Duration: time.Since(time.Time(t)).Truncate(time.Millisecond).String()}
}

func (o Options) get(ctx context.Context, method, url string, hdr http.Header) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error { err := c.ReadCloser.Close(); c.cancel(); return err }

// checkServer probes the DAV listener with an unauthenticated OPTIONS. Any
// HTTP answer proves it is up; auth usually answers 401 before DAV runs.
func checkServer(ctx context.Context, o Options) Result {
	t := startTimer()
	if o.ServerName != "" {
		o.HTTP = withServerName(o.HTTP, o.ServerName)
	}
	resp, err := o.get(ctx, http.MethodOptions, o.ServerURL, nil)
	if err != nil {
		return t.result("calserver", Unhealthy, o.ServerURL, err.Error())
	}
	resp.Body.Close()
	msg := fmt.Sprintf("responding (HTTP %d)", resp.StatusCode)
	switch {
	case resp.Header.Get("DAV") != "":
		msg = "responding (DAV: " + resp.Header.Get("DAV") + ")"
	case resp.StatusCode == http.StatusUnauthorized:
		msg = "responding (auth required)"
	}
	return t.result("calserver", Healthy, o.ServerURL, msg)
}

// withServerName copies c with its transport verifying TLS against name
// instead of the dialed host. Only the DAV probe gets it: the other probes
// share c and reach hosts the certificate does not cover.
func withServerName(c *http.Client, name string) *http.Client {
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == nil {
		if c.Transport != nil {
			return c // a custom RoundTripper we cannot reconfigure
		}
		tr = http.DefaultTransport.(*http.Transport)
	}
	tr = tr.Clone()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.ServerName = name
	cc := *c
	cc.Transport = tr
	return &cc
}

// checkContactsAPI probes the webmail contacts listener without credentials.
// A 401 is the healthy answer: the listener is up and its auth gate is wired.
func checkContactsAPI(ctx context.Context, o Options) Result {
	t := startTimer()
	url := o.APIURL + "/api/v1/health"
	resp, err := o.get(ctx, http.MethodGet, url, nil)
	if err != nil {
		return t.result("contacts-api", Unhealthy, o.APIURL, err.Error())
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return t.result("contacts-api", Healthy, o.APIURL, "responding (auth required)")
	case http.StatusServiceUnavailable:
		return t.result("contacts-api", Unhealthy, o.APIURL, "503: no authentication backend configured")
	default:
		return t.result("contacts-api", Degraded, o.APIURL,
			fmt.Sprintf("unexpected HTTP %d to an unauthenticated probe", resp.StatusCode))
	}
}

// checkDatabase opens a pool the way calserver does, confirms the schema, and
// reads the job queue. It returns the database row and, when the database is
// usable, the jobs row.
func checkDatabase(ctx context.Context, o Options) []Result {
	t := startTimer()
	if o.DSN == "" {
		return []Result{t.result("database", Degraded, "", "not configured (server would run in-memory)")}
	}
	db, err := storage.Open(o.DSN)
	if err != nil {
		return []Result{t.result("database", Unhealthy, "", err.Error())}
	}
	defer db.Close()
	pctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		return []Result{t.result("database", Unhealthy, "", err.Error())}
	}
	if err := (&storage.AdminQueries{DB: db}).SchemaPresent(pctx); err != nil {
		return []Result{t.result("database", Unhealthy, "", "schema check failed: "+err.Error())}
	}
	return []Result{
		t.result("database", Healthy, "", "connected, sabre and calserver schema present"),
		checkJobs(ctx, o, db),
	}
}

// node is one host of a (possibly multi-host) DSN.
type node struct {
	label string
	cfg   *pgx.ConnConfig
}

// dsnNodes splits a DSN into one single-host config per distinct host:port,
// in the order pgx would try them. Fewer than two means there is nothing for
// per-node rows to add over the database row.
func dsnNodes(dsn string) ([]node, error) {
	base, err := pgdial.Config(dsn)
	if err != nil {
		return nil, err
	}
	var out []node
	seen := map[string]bool{}
	add := func(host string, port uint16, fb *pgx.ConnConfig) {
		label := net.JoinHostPort(host, strconv.Itoa(int(port)))
		if seen[label] {
			return // sslmode=prefer lists each host twice (TLS, then plain)
		}
		seen[label] = true
		out = append(out, node{label: label, cfg: fb})
	}
	add(base.Host, base.Port, singleHost(base, base.Host, base.Port, base.TLSConfig != nil))
	for _, f := range base.Fallbacks {
		add(f.Host, f.Port, singleHost(base, f.Host, f.Port, f.TLSConfig != nil))
	}
	return out, nil
}

func singleHost(base *pgx.ConnConfig, host string, port uint16, keepTLS bool) *pgx.ConnConfig {
	c := base.Copy()
	c.Host, c.Port, c.Fallbacks = host, port, nil
	if !keepTLS {
		c.TLSConfig = nil
	}
	for _, f := range base.Fallbacks {
		if f.Host == host && f.Port == port && f.TLSConfig != nil {
			c.TLSConfig = f.TLSConfig
		}
	}
	return c
}

// checkNodes connects to every host of a multi-host DSN on its own. calserver
// tries the hosts in order, so a node that drops traffic costs each fresh
// connection a full connect_timeout before the next is tried — invisible in
// the database row, which only proves that *some* host answers.
func checkNodes(ctx context.Context, o Options) []Result {
	if o.DSN == "" {
		return nil
	}
	nodes, err := dsnNodes(o.DSN)
	if err != nil || len(nodes) < 2 {
		return nil // the database row reports a DSN that does not parse
	}
	out := make([]Result, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = checkNode(ctx, o, n)
		}()
	}
	wg.Wait()

	// One dead node of several is what failover is for: degraded. All dead
	// is an outage.
	up := 0
	for _, r := range out {
		if r.Status == Healthy {
			up++
		}
	}
	for i := range out {
		if out[i].Status != Healthy {
			if up > 0 {
				out[i].Status = Degraded
			} else {
				out[i].Status = Unhealthy
			}
		}
	}
	return out
}

func checkNode(ctx context.Context, o Options, n node) Result {
	t := startTimer()
	component := "database[" + n.label + "]"
	cctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, n.cfg)
	if err != nil {
		return t.result(component, Unhealthy, "", err.Error())
	}
	defer conn.Close(context.Background())
	var standby bool
	if err := conn.QueryRow(cctx, "SELECT pg_is_in_recovery()").Scan(&standby); err != nil {
		return t.result(component, Unhealthy, "", err.Error())
	}
	role := "primary"
	if standby {
		role = "standby (read-only)"
	}
	return t.result(component, Healthy, "", "connected, "+role)
}

// poolStats are the database/sql pool gauges calserver exports under
// db_name="calserver".
type poolStats struct {
	open, inUse, idle, maxOpen, waits float64
	waitSeconds                       float64
	seen                              bool
}

// checkPool reads the running daemon's own pool from /metrics: the one view
// of calserver's database access that a fresh connection from here cannot
// give. A pool pinned at its cap is requests queueing for a connection.
func checkPool(ctx context.Context, o Options) Result {
	t := startTimer()
	resp, err := o.get(ctx, http.MethodGet, o.MetricsURL, nil)
	if err != nil {
		return t.result("db-pool", Unhealthy, o.MetricsURL, "metrics endpoint unreachable: "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return t.result("db-pool", Degraded, o.MetricsURL, fmt.Sprintf("metrics endpoint answered HTTP %d", resp.StatusCode))
	}
	s, err := parsePoolStats(resp.Body)
	if err != nil {
		return t.result("db-pool", Degraded, o.MetricsURL, "reading metrics: "+err.Error())
	}
	if !s.seen {
		return t.result("db-pool", Healthy, o.MetricsURL, "daemon up, no database pool (in-memory)")
	}
	msg := fmt.Sprintf("%.0f/%.0f in use, %.0f idle; %.0f waits (%s total)",
		s.inUse, s.maxOpen, s.idle, s.waits,
		time.Duration(s.waitSeconds*float64(time.Second)).Truncate(time.Millisecond))
	if s.maxOpen > 0 && s.inUse >= s.maxOpen {
		return t.result("db-pool", Degraded, o.MetricsURL, "exhausted: "+msg)
	}
	return t.result("db-pool", Healthy, o.MetricsURL, msg)
}

func parsePoolStats(r io.Reader) (poolStats, error) {
	var s poolStats
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "go_sql_") || !strings.Contains(line, `db_name="calserver"`) {
			continue
		}
		name, rest, _ := strings.Cut(line, "{")
		_, val, _ := strings.Cut(rest, "} ")
		fields := strings.Fields(val)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		s.seen = true
		switch name {
		case "go_sql_open_connections":
			s.open = v
		case "go_sql_in_use_connections":
			s.inUse = v
		case "go_sql_idle_connections":
			s.idle = v
		case "go_sql_max_open_connections":
			s.maxOpen = v
		case "go_sql_wait_count_total":
			s.waits = v
		case "go_sql_wait_duration_seconds_total":
			s.waitSeconds = v
		}
	}
	return s, sc.Err()
}

// Job-queue thresholds: a due job older than staleDue means no worker is
// claiming; an 'executing' claim older than stuckClaim outlived the reclaim
// window (internal/jobs defaultReclaimAfter, 5m) and is not being swept.
const (
	staleDue   = 10 * time.Minute
	stuckClaim = 15 * time.Minute
)

// checkJobs summarizes the durable job queue (iMIP sends): due work nobody is
// claiming and claims nobody is finishing are what a stalled worker looks
// like. Discards are reported but do not degrade: a permanently failing
// invitation is the recipient's problem, not the service's.
func checkJobs(ctx context.Context, o Options, db *sql.DB) Result {
	t := startTimer()
	qctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	var due, stuck, discarded int
	var oldest float64
	err := db.QueryRowContext(qctx, `
SELECT count(*) FILTER (WHERE state = 'available' AND scheduled_at <= now()),
       coalesce(extract(epoch FROM now() - min(scheduled_at)
                FILTER (WHERE state = 'available' AND scheduled_at <= now())), 0),
       count(*) FILTER (WHERE state = 'executing' AND attempted_at < now() - make_interval(secs => $1)),
       count(*) FILTER (WHERE state = 'discarded' AND completed_at > now() - interval '24 hours')
  FROM jobs`, stuckClaim.Seconds()).Scan(&due, &oldest, &stuck, &discarded)
	if err != nil {
		return t.result("jobs", Unhealthy, "", err.Error())
	}
	age := time.Duration(oldest * float64(time.Second)).Truncate(time.Second)
	msg := fmt.Sprintf("%d due", due)
	if due > 0 {
		msg += fmt.Sprintf(" (oldest %s)", age)
	}
	msg += fmt.Sprintf(", %d stuck, %d discarded in 24h", stuck, discarded)
	if stuck > 0 || age > staleDue {
		return t.result("jobs", Degraded, "", msg)
	}
	return t.result("jobs", Healthy, "", msg)
}

// rcptdProbeAddress is valid in form, which rcptd checks before any lookup
// (the domain needs a dot: a bare "@invalid" is a 400), and can never exist:
// .invalid is reserved (RFC 2606), so the healthy answer is a 404.
const rcptdProbeAddress = "calctl-health-probe@health.invalid"

// checkRcptd probes the /auth endpoint calserver's rcptd backend calls, with
// an address that cannot exist, and maps the statuses the way the backend
// does: 404 proves a working service as well as 200; 401 is our token
// rejected; 503 is rcptd's transient table fault.
func checkRcptd(ctx context.Context, o Options) Result {
	t := startTimer()
	hdr := http.Header{}
	if o.RcptdToken != "" {
		hdr.Set("Authorization", "Bearer "+o.RcptdToken)
	}
	resp, err := o.get(ctx, http.MethodGet, o.RcptdURL+"/auth?email="+url.QueryEscape(rcptdProbeAddress), hdr)
	if err != nil {
		return t.result("rcptd", Unhealthy, o.RcptdURL, err.Error())
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		return t.result("rcptd", Healthy, o.RcptdURL, "auth endpoint responding")
	case http.StatusUnauthorized:
		return t.result("rcptd", Unhealthy, o.RcptdURL,
			"bearer token rejected — check [server.rcptd] token / $RCPTD_AUTH_TOKEN")
	case http.StatusServiceUnavailable:
		return t.result("rcptd", Degraded, o.RcptdURL, "temporarily unavailable (transient table fault)")
	case http.StatusBadRequest:
		return t.result("rcptd", Unhealthy, o.RcptdURL, "rejected the probe address as malformed (HTTP 400)")
	default:
		return t.result("rcptd", Unhealthy, o.RcptdURL, fmt.Sprintf("unexpected HTTP %d from /auth", resp.StatusCode))
	}
}
