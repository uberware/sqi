// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/metrics"
)

func TestNew_ServesRecordedMetric(t *testing.T) {
	m := metrics.New()
	if m == nil {
		t.Fatal("New returned nil")
	}
	// HTTPRequestsTotal is labeled (method, path, status_code) per metrics.go.
	m.HTTPRequestsTotal.WithLabelValues("GET", "/api/v1/jobs", "200").Inc()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sqi_http_requests_total") {
		t.Errorf("exposition missing sqi_http_requests_total:\n%s", rec.Body.String())
	}
}

func TestNew_TwiceDoesNotPanic(_ *testing.T) {
	_ = metrics.New()
	_ = metrics.New()
}
