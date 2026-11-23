// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/metrics"
)

// unroutedPath is the "path" label of a request answered before chi matched
// any route pattern: an HA standby's 503, or chi's 405 for a method it does
// not know.
const unroutedPath = "unrouted"

// otherMethod is the "method" label of a request whose method is not one of
// the standard HTTP methods. net/http accepts any token as a method, so the
// raw value would let a client add series without bound.
const otherMethod = "OTHER"

// RequestMetrics returns middleware that records Prometheus metrics for every
// HTTP request:
//
//   - sqi_http_requests_total — counter incremented after the handler returns,
//     labeled by method, path, and status code.
//   - sqi_http_request_duration_seconds — histogram observed after the handler
//     returns, labeled by method and path.
//
// The "path" label is the chi route pattern that served the request, such as
// /api/v1/jobs/{id}, so the number of series is bounded by the routes, not by
// the IDs requested. A request a sub-router answered without matching one of
// its routes carries the sub-router's mount pattern (/api/v1/* for an
// unknown API path or a rate-limited request, /* for the UI catch-all). The
// "method" label is the request method, or OTHER for a non-standard one.
// RequestMetrics must be mounted on a chi router: the pattern is read from
// the request's chi route context after the handler returns.
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
			method := methodLabel(r.Method)
			path := routePattern(r)
			statusCode := strconv.Itoa(rw.status)

			m.HTTPRequestsTotal.
				WithLabelValues(method, path, statusCode).
				Inc()

			m.HTTPRequestDuration.
				WithLabelValues(method, path).
				Observe(elapsed.Seconds())
		})
	}
}

// routePattern returns the route pattern chi matched for r, or unroutedPath.
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return unroutedPath
}

// methodLabel returns method if it is a standard HTTP method, or otherMethod.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	}
	return otherMethod
}
