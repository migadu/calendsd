package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/migadu/calendsd/internal/metrics"
)

func TestWrapMetricsNilSet(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := WrapMetrics(inner, nil, "/"); !isSameHandler(got, inner) {
		t.Error("nil set should return next unchanged")
	}
}

func isSameHandler(a http.Handler, b http.HandlerFunc) bool {
	af, ok := a.(http.HandlerFunc)
	return ok && fmt.Sprintf("%p", af) == fmt.Sprintf("%p", b)
}

func TestWrapMetricsRecords(t *testing.T) {
	set := metrics.New("test")
	h := WrapMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, "body")
	}), set, "/")

	req := httptest.NewRequest("PROPFIND", "/calendars/hans/default", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	want := `
# HELP calserver_http_requests_total DAV requests served, by normalized method, route pattern, and status code.
# TYPE calserver_http_requests_total counter
calserver_http_requests_total{code="207",method="PROPFIND",route="calendar"} 1
`
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(want),
		"calserver_http_requests_total"); err != nil {
		t.Error(err)
	}
	if got := testutil.CollectAndCount(set.Registry()); got == 0 {
		t.Error("registry gathered no series")
	}
}

func TestWrapMetricsImplicit200(t *testing.T) {
	set := metrics.New("test")
	h := WrapMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok") // no explicit WriteHeader
	}), set, "/")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	want := `
# HELP calserver_http_requests_total DAV requests served, by normalized method, route pattern, and status code.
# TYPE calserver_http_requests_total counter
calserver_http_requests_total{code="200",method="GET",route="root"} 1
`
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(want),
		"calserver_http_requests_total"); err != nil {
		t.Error(err)
	}
}

// TestWrapMetricsPanicCounted: a panicking handler must still be observed
// (as a 500) — a crashing request class must not read as a healthy dashboard.
func TestWrapMetricsPanicCounted(t *testing.T) {
	set := metrics.New("test")
	h := WrapMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}), set, "/")

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed; it must propagate to net/http")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("REPORT", "/calendars/hans/default", nil))
	}()

	want := `
# HELP calserver_http_requests_total DAV requests served, by normalized method, route pattern, and status code.
# TYPE calserver_http_requests_total counter
calserver_http_requests_total{code="500",method="REPORT",route="calendar"} 1
`
	if err := testutil.GatherAndCompare(set.Registry(), strings.NewReader(want),
		"calserver_http_requests_total"); err != nil {
		t.Error(err)
	}
}

// TestWrapMetricsCardinality is the cardinality guard (docs/design/METRICS-PLAN.md §7): a
// hostile spread of methods and paths must not grow the label sets beyond the
// closed method x route x code space.
func TestWrapMetricsCardinality(t *testing.T) {
	set := metrics.New("test")
	h := WrapMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}), set, "/")

	for i := 0; i < 500; i++ {
		method := fmt.Sprintf("METHOD%d", i)
		path := fmt.Sprintf("/calendars/user%d/cal%d/obj%d.ics", i, i, i)
		if i%3 == 0 {
			path = fmt.Sprintf("/scan/%d/%s", i, strings.Repeat("x", i%50))
		}
		req := httptest.NewRequest(method, path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	// All 500 requests share method="other" and at most two routes
	// (calendar-object, other) and one code: <= 2 counter series plus <= 2
	// histogram series (each histogram series gathers as one metric family
	// entry; CollectAndCount counts series, with histograms as 1 each).
	reqSeries := testutil.CollectAndCount(set.Registry(), "calserver_http_requests_total")
	durSeries := testutil.CollectAndCount(set.Registry(), "calserver_http_request_duration_seconds")
	if reqSeries > 2 || durSeries > 2 {
		t.Errorf("hostile requests grew label sets: %d counter series, %d histogram series",
			reqSeries, durSeries)
	}
}
