package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/observability"
)

// scrapeServer renders the exposition from a server's registry.
func scrapeServer(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// The label must be the route *pattern*. Recording the concrete path would put
// page slugs and workspace ids into label values, and unbounded label values
// are how the monitoring system becomes the outage.
func TestMetricsMiddlewareLabelsByRoutePattern(t *testing.T) {
	m := observability.NewMetrics()
	s := &Server{Writes: (*fakeWrites)(nil), Metrics: m}
	srv := httptest.NewServer(s.Router())
	defer srv.Close()

	// A route with a path parameter, requested with a concrete value.
	res, err := http.Get(srv.URL + "/api/v1/version")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	body := scrapeServer(t, m)
	if !strings.Contains(body, `route="/api/v1/version"`) {
		t.Errorf("exposition missing the version route:\n%s", body)
	}
	if !strings.Contains(body, `status="2xx"`) {
		t.Error("status class not recorded")
	}
	// Duration is observed alongside the counter, or latency alerts have
	// nothing to fire on.
	if !strings.Contains(body, "kiln_http_request_duration_seconds_count") {
		t.Error("duration histogram not recorded")
	}
}

func TestMetricsMiddlewareRecordsNotFoundAsOther(t *testing.T) {
	m := observability.NewMetrics()
	s := &Server{Writes: (*fakeWrites)(nil), Metrics: m}
	srv := httptest.NewServer(s.Router())
	defer srv.Close()

	// An unmatched API path has no route pattern. It must still be counted --
	// a flood of 404s is a signal -- but under a single bounded label rather
	// than one series per bogus path an attacker cares to invent.
	for _, p := range []string{"/api/v1/nope-one", "/api/v1/nope-two"} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}

	body := scrapeServer(t, m)
	if !strings.Contains(body, `status="4xx"`) {
		t.Errorf("unmatched paths not counted:\n%s", body)
	}
	if strings.Contains(body, "nope-one") || strings.Contains(body, "nope-two") {
		t.Error("unmatched concrete paths leaked into labels")
	}
}

// The recorder wraps the ResponseWriter, so it must not break the interfaces
// the upload path depends on: http.ResponseController reaches the real writer
// through Unwrap to extend deadlines for a 32 MiB stream.
func TestStatusRecorderPreservesResponseWriterCapabilities(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	if sr.Unwrap() != http.ResponseWriter(rec) {
		t.Error("Unwrap did not return the wrapped writer")
	}

	// First status wins: a handler that writes a body after WriteHeader must
	// not overwrite the recorded code.
	sr.WriteHeader(http.StatusTeapot)
	sr.WriteHeader(http.StatusOK)
	if sr.status != http.StatusTeapot {
		t.Errorf("status = %d, want the first one written", sr.status)
	}

	// A bare Write implies 200 and must not be re-recorded later.
	rec2 := httptest.NewRecorder()
	sr2 := &statusRecorder{ResponseWriter: rec2, status: http.StatusOK}
	if _, err := sr2.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	sr2.WriteHeader(http.StatusInternalServerError)
	if sr2.status != http.StatusOK {
		t.Errorf("status = %d, want 200 from the implicit header", sr2.status)
	}
	sr2.Flush() // must not panic on a writer that supports it
}
