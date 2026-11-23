// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store/fake"
)

// TestNewRouter_RequestMetricsLabels pins the method and path labels the
// production router gives its HTTP metrics. They depend on where
// RequestMetrics, the HA standby gate and the /api/v1 sub-router are mounted
// relative to each other.
func TestNewRouter_RequestMetricsLabels(t *testing.T) {
	st := fake.New()
	deps := Deps{Store: st, Products: product.NewCatalog(st)}
	standby := &HAGate{IsLeader: func() bool { return false }, LeaderName: func() string { return "alpha" }}

	tests := []struct {
		name                 string
		ha                   *HAGate
		method, target       string
		wantMethod, wantPath string
	}{
		{"ID route", nil, http.MethodGet, "/api/v1/jobs/j-123", http.MethodGet, "/api/v1/jobs/{id}"},
		{"unknown API path", nil, http.MethodGet, "/api/v1/no-such-route", http.MethodGet, "/api/v1/*"},
		{"non-standard method", nil, "XYZ", "/api/v1/version", "OTHER", "unrouted"},
		{"HA standby", standby, http.MethodGet, "/api/v1/version", http.MethodGet, "unrouted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New()
			r := NewRouter(Config{DisableRateLimit: true, HA: tc.ha}, deps, newTestLogger(), m, health.NewRegistry())
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), tc.method, tc.target, nil))

			if n := testutil.CollectAndCount(m.HTTPRequestsTotal); n != 1 {
				t.Fatalf("sqi_http_requests_total has %d series, want 1", n)
			}
			code := strconv.Itoa(rr.Code)
			if got := testutil.ToFloat64(m.HTTPRequestsTotal.WithLabelValues(tc.wantMethod, tc.wantPath, code)); got != 1 {
				t.Errorf("sqi_http_requests_total{method=%q,path=%q,status_code=%q} = %v, want 1",
					tc.wantMethod, tc.wantPath, code, got)
			}
		})
	}
}
