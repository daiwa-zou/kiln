package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/observability"
)

// metricsMiddleware records every served request.
//
// The route label is chi's matched *pattern*, resolved after the handler runs
// because that is when the router has finished matching. Using the concrete
// path instead would label a metric with page slugs and workspace ids --
// unbounded values, and unbounded label values are how the monitoring system
// becomes the outage.
func metricsMiddleware(m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			done := m.InFlight()
			defer done()

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r)

			route := "other"
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				if pattern := rctx.RoutePattern(); pattern != "" {
					route = pattern
				}
			}
			m.ObserveHTTP(r.Method, route, rec.status, time.Since(start))
		})
	}
}

// statusRecorder captures the status code for the metric. It forwards
// Flush and Unwrap so the streaming upload path and http.ResponseController
// (which the upload handler uses to extend deadlines) keep working through
// the wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer. Without it, the
// upload handler's SetReadDeadline/SetWriteDeadline would silently stop
// working the moment this middleware wrapped it.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
