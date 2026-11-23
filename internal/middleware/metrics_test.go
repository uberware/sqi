// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/middleware"
)

// TestRequestMetrics_LabelsByRoutePattern checks that requests for different
// IDs share one series labeled with the route pattern, so the series count is
// bounded by the routes.
func TestRequestMetrics_LabelsByRoutePattern(t *testing.T) {
	m := metrics.New()
	r := chi.NewRouter()
	r.Use(middleware.RequestMetrics(m))
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/jobs/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/jobs/a"},
		{http.MethodGet, "/api/v1/jobs/b"},
		{http.MethodGet, "/api/v1/jobs/c"},
		{http.MethodGet, "/no/such/route"},
		{"XYZ1", "/api/v1/jobs/a"},
		{"XYZ2", "/api/v1/jobs/a"},
	} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), req.method, req.path, nil))
	}

	if n := testutil.CollectAndCount(m.HTTPRequestsTotal); n != 3 {
		t.Errorf("sqi_http_requests_total has %d series, want 3", n)
	}
	tests := []struct {
		method, path, status string
		want                 float64
	}{
		{http.MethodGet, "/api/v1/jobs/{id}", "200", 3},
		{http.MethodGet, "unrouted", "404", 1},
		{"OTHER", "unrouted", "405", 2},
	}
	for _, tc := range tests {
		got := testutil.ToFloat64(m.HTTPRequestsTotal.WithLabelValues(tc.method, tc.path, tc.status))
		if got != tc.want {
			t.Errorf("sqi_http_requests_total{method=%q,path=%q,status_code=%q} = %v, want %v",
				tc.method, tc.path, tc.status, got, tc.want)
		}
	}
}
