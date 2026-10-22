// Package metrics defines every Prometheus instrument calendsd exports in
// one place, so names, labels, and buckets stay auditable (METRICS-PLAN.md).
// All series are named calserver_* and live on the Set's own registry, never
// the client library's global default.
//
// A nil *Set is valid everywhere and records nothing, so instrumented code
// paths need no registry in tests or in binaries that don't scrape. Every
// label value recorded here comes from a closed set computed by the caller —
// never from request data — which is what keeps series cardinality bounded.
package metrics

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Set owns the registry and all instruments.
type Set struct {
	registry *prometheus.Registry

	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	httpInFlight   prometheus.Gauge
	reportRequests *prometheus.CounterVec

	authAttempts      *prometheus.CounterVec
	authCheckDuration *prometheus.HistogramVec
	rcptdRequests     *prometheus.CounterVec
	rcptdDuration     prometheus.Histogram
	authCacheEvents   *prometheus.CounterVec

	dbOpDuration  *prometheus.HistogramVec
	syncTruncated *prometheus.CounterVec

	imipDeliveries *prometheus.CounterVec
	imipDuration   prometheus.Histogram
	shareNotify    *prometheus.CounterVec
}

// New builds a Set with every instrument registered, plus the Go runtime and
// process collectors and a constant calserver_build_info{version} series.
func New(version string) *Set {
	reg := prometheus.NewRegistry()
	s := &Set{
		registry: reg,

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "calserver_http_requests_total",
			Help: "DAV requests served, by normalized method, route pattern, and status code.",
		}, []string{"method", "route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "calserver_http_request_duration_seconds",
			Help: "DAV request duration. Sync of a large calendar legitimately takes seconds.",
			// Wider than the defaults: a busy sync-collection or multiget can
			// legitimately run for seconds.
			Buckets: []float64{0.005, 0.025, 0.1, 0.25, 1, 2.5, 10, 30},
		}, []string{"method", "route"}),
		httpInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "calserver_http_requests_in_flight",
			Help: "Requests currently being served.",
		}),
		reportRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "calserver_report_requests_total",
			Help: "REPORT requests by report type (REPORT is a catch-all verb).",
		}, []string{"type"}),

		authAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "calserver_auth_attempts_total",
			Help: "Authentication attempts by backend and typed outcome.",
		}, []string{"backend", "outcome"}),
		authCheckDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "calserver_auth_check_duration_seconds",
			Help: "Whole credential-check duration per backend, including local hash verification.",
			// The local bcrypt verify is often the dominant term by design.
			Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		}, []string{"backend"}),
		rcptdRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "calserver_rcptd_requests_total",
			Help: "HTTP lookups against rcptd /auth by status code (or \"error\" for transport failures).",
		}, []string{"code"}),
		rcptdDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "calserver_rcptd_request_duration_seconds",
			Help: "rcptd /auth roundtrip duration.",
			// The rcptd client times out at 5s.
			Buckets: []float64{0.001, 0.005, 0.025, 0.1, 0.5, 1, 5},
		}),
		authCacheEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "calserver_auth_cache_events_total",
			Help: "Positive-credential cache consultations (hit skips the rcptd roundtrip and hash verify).",
		}, []string{"result"}),

		dbOpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "calserver_db_op_duration_seconds",
			Help:    "Duration of named expensive storage operations (not every query).",
			Buckets: []float64{0.001, 0.005, 0.025, 0.1, 0.5, 2, 10},
		}, []string{"op"}),
		syncTruncated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "calserver_sync_responses_truncated_total",
			Help: "sync-collection responses truncated by the client's limit; frequent truncation means pathological re-syncs.",
		}, []string{"resource"}),

		imipDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "calserver_imip_deliveries_total",
			Help: "External iTIP deliveries by outcome; smtp_error, auth_error, and retry_exhausted are alert-worthy (delivery/auth infrastructure failures). rate_limited, not_authorized_sender, and no_credential are expected client-side outcomes. A message that is queued for retry counts once as queued and once more at its terminal outcome.",
		}, []string{"outcome"}),
		imipDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "calserver_imip_delivery_duration_seconds",
			Help:    "SMTP send duration for iMip messages (the Mailer.Send call only).",
			Buckets: []float64{0.01, 0.05, 0.25, 1, 5, 15},
		}),
		shareNotify: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "calserver_share_notify_total",
			Help: "Calendar-share notice emails by outcome (docs/share-invite-email.md). queued counts accepted enqueues (including coalesced no-ops), sent counts SMTP successes. enqueue_error, policy_error, lookup_error, smtp_error, and retry_exhausted are alert-worthy; skipped_* are expected.",
		}, []string{"outcome"}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "calserver_build_info",
		Help: "Constant 1, labeled by build version, to correlate regressions with deploys.",
	}, []string{"version"})
	if version == "" {
		version = "dev"
	}
	buildInfo.WithLabelValues(version).Set(1)

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		s.httpRequests, s.httpDuration, s.httpInFlight, s.reportRequests,
		s.authAttempts, s.authCheckDuration, s.rcptdRequests, s.rcptdDuration,
		s.authCacheEvents,
		s.dbOpDuration, s.syncTruncated,
		s.imipDeliveries, s.imipDuration, s.shareNotify,
	)
	return s
}

// Handler serves the registry in the Prometheus exposition format. A nil Set
// serves 404, upholding the nil-is-valid contract for callers that wire the
// handler unconditionally.
func (s *Set) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})
}

// Registry exposes the underlying registry (tests); nil for a nil Set.
func (s *Set) Registry() *prometheus.Registry {
	if s == nil {
		return nil
	}
	return s.registry
}

// RegisterDBStats adds the sql.DBStats pool collector (in-use, idle, wait
// count and duration — wait duration rising is the earliest saturation
// signal).
func (s *Set) RegisterDBStats(db *sql.DB) {
	if s == nil {
		return
	}
	s.registry.MustRegister(collectors.NewDBStatsCollector(db, "calserver"))
}

// RegisterJobDepth adds a collector reporting the durable job-queue depth by
// state (calserver_jobs_total{state}), queried once per scrape. A rising
// 'available' depth means the workers are falling behind; a rising 'discarded'
// count means sends are giving up. Safe when the jobs table is absent (a
// sabre-only database): the query fails and the collector reports nothing.
func (s *Set) RegisterJobDepth(db *sql.DB) {
	if s == nil {
		return
	}
	s.registry.MustRegister(&jobDepthCollector{db: db})
}

type jobDepthCollector struct{ db *sql.DB }

var jobDepthDesc = prometheus.NewDesc(
	"calserver_jobs_total",
	"Durable background jobs by state (available|executing|completed|discarded), sampled per scrape.",
	[]string{"state"}, nil)

func (c *jobDepthCollector) Describe(ch chan<- *prometheus.Desc) { ch <- jobDepthDesc }

func (c *jobDepthCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return // best-effort: a scrape must not fail the whole endpoint
	}
	defer rows.Close()
	for rows.Next() {
		var (
			state string
			n     float64
		)
		if err := rows.Scan(&state, &n); err != nil {
			return
		}
		ch <- prometheus.MustNewConstMetric(jobDepthDesc, prometheus.GaugeValue, n, state)
	}
}

// ObserveHTTPRequest records one served request. method and route must come
// from NormalizeMethod and RoutePattern.
func (s *Set) ObserveHTTPRequest(method, route string, code int, seconds float64) {
	if s == nil {
		return
	}
	s.httpRequests.WithLabelValues(method, route, strconv.Itoa(code)).Inc()
	s.httpDuration.WithLabelValues(method, route).Observe(seconds)
}

// HTTPInFlightInc / HTTPInFlightDec track currently-running requests.
func (s *Set) HTTPInFlightInc() {
	if s == nil {
		return
	}
	s.httpInFlight.Inc()
}

func (s *Set) HTTPInFlightDec() {
	if s == nil {
		return
	}
	s.httpInFlight.Dec()
}

// ReportRequest counts one REPORT by its type label (a closed set the DAV
// core computes from the report's root element).
func (s *Set) ReportRequest(reportType string) {
	if s == nil {
		return
	}
	s.reportRequests.WithLabelValues(reportType).Inc()
}

// AuthAttempt counts one authentication attempt. backend is basic, digest,
// rcptd, or none (a credential-less probe); outcome is one of ok,
// bad_credentials, unavailable, no_credentials, provision_error, error.
func (s *Set) AuthAttempt(backend, outcome string) {
	if s == nil {
		return
	}
	s.authAttempts.WithLabelValues(backend, outcome).Inc()
}

// ObserveAuthCheck records the whole credential-check duration for a backend.
func (s *Set) ObserveAuthCheck(backend string, seconds float64) {
	if s == nil {
		return
	}
	s.authCheckDuration.WithLabelValues(backend).Observe(seconds)
}

// RcptdRequest records one /auth roundtrip; code is the HTTP status as a
// string, or "error" for transport failures.
func (s *Set) RcptdRequest(code string, seconds float64) {
	if s == nil {
		return
	}
	s.rcptdRequests.WithLabelValues(code).Inc()
	s.rcptdDuration.Observe(seconds)
}

// AuthCacheEvent counts a credential-cache consultation: "hit" or "miss".
func (s *Set) AuthCacheEvent(result string) {
	if s == nil {
		return
	}
	s.authCacheEvents.WithLabelValues(result).Inc()
}

// DBOpTimer starts timing a named storage operation; the returned func
// records the observation, so callers can `defer m.DBOpTimer("op")()`.
func (s *Set) DBOpTimer(op string) func() {
	if s == nil {
		return func() {}
	}
	t := prometheus.NewTimer(s.dbOpDuration.WithLabelValues(op))
	return func() { t.ObserveDuration() }
}

// SyncTruncated counts a limit-truncated sync-collection response; resource
// is "calendar", "addressbook", or "other".
func (s *Set) SyncTruncated(resource string) {
	if s == nil {
		return
	}
	s.syncTruncated.WithLabelValues(resource).Inc()
}

// IMipDelivery counts one external delivery outcome. Relay path: sent,
// smtp_error, no_mailer. Per-user submission path: sent_as_user,
// resource_relay, not_authorized_sender, no_credential, auth_error,
// rate_limited. Either path: invalid_recipient, unsupported, skipped; and
// with the retry queue: queued when a transient failure is handed to it,
// queue_full when it refuses one, then the success outcome or
// retry_exhausted when it resolves.
func (s *Set) IMipDelivery(outcome string) {
	if s == nil {
		return
	}
	s.imipDeliveries.WithLabelValues(outcome).Inc()
}

// ShareNotify counts one share-notice outcome: queued, sent, skipped_policy,
// skipped_obsolete, skipped_excluded, invalid_recipient, no_mailer,
// no_web_url, policy_error, lookup_error, enqueue_error, smtp_error, or
// retry_exhausted.
func (s *Set) ShareNotify(outcome string) {
	if s == nil {
		return
	}
	s.shareNotify.WithLabelValues(outcome).Inc()
}

// ObserveIMipSend records the SMTP send duration (observed whether or not
// the send succeeded — timeouts are the interesting case).
func (s *Set) ObserveIMipSend(seconds float64) {
	if s == nil {
		return
	}
	s.imipDuration.Observe(seconds)
}
