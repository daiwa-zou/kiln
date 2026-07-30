package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// scrape renders the exposition the way Prometheus would read it.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}

func TestHTTPMetrics(t *testing.T) {
	m := NewMetrics()
	m.ObserveHTTP(http.MethodGet, "/api/v1/workspaces/{workspace}/pages", 200, 12*time.Millisecond)
	m.ObserveHTTP(http.MethodGet, "/api/v1/workspaces/{workspace}/pages", 500, time.Second)
	m.ObserveHTTP(http.MethodPost, "/api/v1/workspaces/{workspace}/files", 413, 2*time.Second)

	body := scrape(t, m)
	for _, want := range []string{
		`kiln_http_requests_total{method="GET",route="/api/v1/workspaces/{workspace}/pages",status="2xx"} 1`,
		`kiln_http_requests_total{method="GET",route="/api/v1/workspaces/{workspace}/pages",status="5xx"} 1`,
		`kiln_http_requests_total{method="POST",route="/api/v1/workspaces/{workspace}/files",status="4xx"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}

	// Status is bucketed by class: the exact code is rarely what an alert
	// fires on, and every extra value is cardinality nobody chose.
	if strings.Contains(body, `status="413"`) {
		t.Error("exact status codes leaked into labels")
	}
}

func TestInFlightBrackets(t *testing.T) {
	m := NewMetrics()
	done := m.InFlight()
	if got := testutil.ToFloat64(m.httpInFlight); got != 1 {
		t.Errorf("in flight during request = %v, want 1", got)
	}
	done()
	if got := testutil.ToFloat64(m.httpInFlight); got != 0 {
		t.Errorf("in flight after request = %v, want 0", got)
	}
}

func TestRunMetrics(t *testing.T) {
	m := NewMetrics()
	m.RunStarted("webhook")
	if got := testutil.ToFloat64(m.activeRuns); got != 1 {
		t.Errorf("active runs after start = %v, want 1", got)
	}
	m.RunFinished("succeeded", 90*time.Second, 0.42, 3, 5, 1)
	if got := testutil.ToFloat64(m.activeRuns); got != 0 {
		t.Errorf("active runs after finish = %v, want 0", got)
	}

	body := scrape(t, m)
	for _, want := range []string{
		`kiln_runs_started_total{trigger="webhook"} 1`,
		`kiln_runs_finished_total{status="succeeded"} 1`,
		`kiln_pages_total{action="created"} 3`,
		`kiln_pages_total{action="updated"} 5`,
		`kiln_pages_total{action="deleted"} 1`,
		"kiln_run_cost_usd_total 0.42",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}

	// A free run must not inflate the cost counter -- the whole promise of
	// the hash gate is that an unchanged bench costs nothing, and the metric
	// has to be able to show that.
	m.RunFinished("no_changes", time.Second, 0, 0, 0, 0)
	if got := testutil.ToFloat64(m.runCost); got != 0.42 {
		t.Errorf("cost after a free run = %v, want 0.42", got)
	}
}

func TestQueueDepthZeroesStaleStatuses(t *testing.T) {
	m := NewMetrics()
	m.SetQueueDepth(map[string]int{"queued": 7, "running": 2})
	if !strings.Contains(scrape(t, m), `kiln_queue_depth{status="queued"} 7`) {
		t.Error("queue depth not published")
	}

	// A drained queue must read as empty. Leaving the last sample in place
	// would keep an autoscaler holding workers for work that is long done.
	m.SetQueueDepth(map[string]int{"queued": 0, "running": 0})
	body := scrape(t, m)
	if !strings.Contains(body, `kiln_queue_depth{status="queued"} 0`) {
		t.Error("drained queue did not publish zero")
	}
	if strings.Contains(body, `kiln_queue_depth{status="queued"} 7`) {
		t.Error("stale queue depth survived a resample")
	}
}

func TestUnknownLabelsAreNamed(t *testing.T) {
	// An empty label value would render as trigger="" and read as a bug in
	// the dashboard rather than a gap in the data.
	m := NewMetrics()
	m.RunStarted("")
	m.UnitObserved("")
	body := scrape(t, m)
	if !strings.Contains(body, `trigger="unknown"`) || !strings.Contains(body, `outcome="unknown"`) {
		t.Error("empty labels were not normalized to unknown")
	}
}

func TestServeMetricsListenerLifecycle(t *testing.T) {
	m := NewMetrics()
	ctx, cancel := context.WithCancel(context.Background())

	errc := make(chan error, 1)
	go func() { errc <- m.ServeMetrics(ctx, "127.0.0.1:0") }()

	// Canceling the context must shut the listener down cleanly rather than
	// reporting the closure as a failure -- a noisy shutdown trains operators
	// to ignore the logs.
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("shutdown returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("metrics listener did not shut down")
	}
}
