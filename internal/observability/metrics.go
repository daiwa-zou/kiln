package observability

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is kiln's instrumentation, in its own registry rather than the
// default one so nothing a dependency happens to register leaks into the
// exposition, and so tests can assert against a clean set.
//
// What is measured follows from what an operator has to answer at 3am: is the
// API serving, is the queue draining, are builds failing, and what is this
// costing. Cost is a first-class metric here because kiln spends real money
// per run -- a build platform whose spend is only visible in a database table
// cannot be alerted on.
type Metrics struct {
	registry *prometheus.Registry

	// API
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpInFlight prometheus.Gauge

	// Builds
	runsStarted  *prometheus.CounterVec
	runsFinished *prometheus.CounterVec
	runDuration  *prometheus.HistogramVec
	runCost      prometheus.Counter
	unitsBuilt   *prometheus.CounterVec
	pagesWritten *prometheus.CounterVec
	activeRuns   prometheus.Gauge

	// Queue depth, sampled rather than scraped from the database on demand:
	// a scrape must not become a database query multiplier when several
	// Prometheus replicas point at several kiln replicas.
	queueDepth *prometheus.GaugeVec
}

// NewMetrics builds the registry and every collector.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kiln_http_requests_total",
			Help: "HTTP requests by method, route template, and status class.",
		}, []string{"method", "route", "status"}),

		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kiln_http_request_duration_seconds",
			Help: "HTTP request latency by route template.",
			// Reads are milliseconds; uploads stream tens of megabytes. One
			// set of buckets has to cover both, so it runs wider than a
			// default web histogram.
			Buckets: []float64{0.005, 0.025, 0.1, 0.5, 1, 5, 15, 60},
		}, []string{"method", "route"}),

		httpInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kiln_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),

		runsStarted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kiln_runs_started_total",
			Help: "Builds claimed by a worker, by what triggered them.",
		}, []string{"trigger"}),

		runsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kiln_runs_finished_total",
			Help: "Builds that reached a terminal state, by status.",
		}, []string{"status"}),

		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kiln_run_duration_seconds",
			Help: "Wall-clock duration of a build, by status.",
			// Builds are LLM-bound: seconds at the fast end, many minutes for
			// a first full build of a large repository.
			Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600},
		}, []string{"status"}),

		runCost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kiln_run_cost_usd_total",
			Help: "Cumulative model spend across builds, in USD.",
		}),

		unitsBuilt: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kiln_units_total",
			Help: "Units the pipeline resolved, by outcome. Units gated by the content hash are 'unchanged' and cost nothing.",
		}, []string{"outcome"}),

		pagesWritten: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kiln_pages_total",
			Help: "Wiki pages by what happened to them.",
		}, []string{"action"}),

		activeRuns: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kiln_active_runs",
			Help: "Builds in flight in this process. A worker claims one at a time, so this is 0 or 1 per worker.",
		}),

		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kiln_queue_depth",
			Help: "Runs waiting or executing, by status. The signal to scale workers on.",
		}, []string{"status"}),
	}

	reg.MustRegister(
		m.httpRequests, m.httpDuration, m.httpInFlight,
		m.runsStarted, m.runsFinished, m.runDuration, m.runCost,
		m.unitsBuilt, m.pagesWritten, m.activeRuns, m.queueDepth,
		// Go runtime and process collectors: the questions "is it leaking"
		// and "is it being throttled" are asked of every service, and
		// answering them here costs nothing.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Registry exposes the collectors for tests and for a caller that wants to
// mount the handler itself.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler serves the exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A scrape failing loudly is better than one silently reporting a
		// partial set that hides the problem being scraped for.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// ObserveHTTP records one served request. route is the chi route *template*
// ("/api/v1/workspaces/{workspace}/pages"), never the concrete path: pages,
// slugs, and workspace ids are unbounded, and a label with unbounded values is
// how a metrics backend gets taken down by the thing monitoring it.
func (m *Metrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	if route == "" {
		route = "other"
	}
	m.httpRequests.WithLabelValues(method, route, statusClass(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// InFlight brackets a request for the in-flight gauge.
func (m *Metrics) InFlight() func() {
	m.httpInFlight.Inc()
	return m.httpInFlight.Dec
}

// RunStarted records a claimed build.
func (m *Metrics) RunStarted(trigger string) {
	m.runsStarted.WithLabelValues(orUnknown(trigger)).Inc()
	m.activeRuns.Inc()
}

// RunFinished records a terminal build with what it produced and spent.
func (m *Metrics) RunFinished(status string, d time.Duration, costUSD float64, created, updated, deleted int) {
	status = orUnknown(status)
	m.runsFinished.WithLabelValues(status).Inc()
	m.runDuration.WithLabelValues(status).Observe(d.Seconds())
	m.activeRuns.Dec()
	if costUSD > 0 {
		m.runCost.Add(costUSD)
	}
	for action, n := range map[string]int{"created": created, "updated": updated, "deleted": deleted} {
		if n > 0 {
			m.pagesWritten.WithLabelValues(action).Add(float64(n))
		}
	}
}

// UnitObserved records one unit's outcome. "unchanged" is the interesting one:
// it is the content-hash gate working, and the ratio of unchanged to generated
// is what tells an operator whether incremental builds are actually paying off.
func (m *Metrics) UnitObserved(outcome string) {
	m.unitsBuilt.WithLabelValues(orUnknown(outcome)).Inc()
}

// SetQueueDepth publishes the sampled queue. Statuses absent from the sample
// are zeroed rather than left stale, so a drained queue reads as empty instead
// of holding its last non-zero value forever.
func (m *Metrics) SetQueueDepth(byStatus map[string]int) {
	m.queueDepth.Reset()
	for status, n := range byStatus {
		m.queueDepth.WithLabelValues(status).Set(float64(n))
	}
}

// ServeMetrics runs the metrics listener until the context is canceled.
//
// It is a separate listener from the API on purpose: metrics describe the
// deployment, not the tenant, and putting them on the public router would mean
// either exposing queue depth and spend to every reader or inventing an
// authentication scheme Prometheus does not want to use. A separate port is
// trivially firewalled and is what every scrape configuration expects.
func (m *Metrics) ServeMetrics(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	// Cheap endpoints on the same listener, so a sidecar or a probe that
	// cannot reach the API port still has something to ask.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// statusClass buckets a status code as 2xx/4xx/5xx. The exact code is rarely
// what an alert fires on, and the cardinality is not worth the little it adds.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return strconv.Itoa(status)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
