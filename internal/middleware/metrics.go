// SPDX-License-Identifier: AGPL-3.0-only

package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/uberware/sqi/internal/metrics"
)

// RequestMetrics returns middleware that records Prometheus metrics for every
// HTTP request:
//
//   - sqi_http_requests_total — counter incremented after the handler returns,
//     labeled by method, path, and status code.
//   - sqi_http_request_duration_seconds — histogram observed after the handler
//     returns, labeled by method and path.
//
// The "path" label is currently set to r.URL.Path. Once REST endpoints with
// path parameters are added (tasks 71+), replace this with chi's route pattern
// via chi.RouteContext(r.Context()).RoutePattern() to prevent high cardinality
// from parameterised segments such as /api/v1/jobs/{id}.
func RequestMetrics(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap the writer to capture the status code written by the handler.
			// responseWriter is defined in log.go (same package) and also
			// implements http.Flusher and http.Hijacker transparently.
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rw, r)

			elapsed := time.Since(start)
			path := r.URL.Path
			statusCode := strconv.Itoa(rw.status)

			m.HTTPRequestsTotal.
				WithLabelValues(r.Method, path, statusCode).
				Inc()

			m.HTTPRequestDuration.
				WithLabelValues(r.Method, path).
				Observe(elapsed.Seconds())
		})
	}
}
