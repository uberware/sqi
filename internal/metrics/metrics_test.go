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

// TestNew_ServesHAMetrics pins the HA metric names the operations docs tell
// operators to alert on and to select the leader with.
func TestNew_ServesHAMetrics(t *testing.T) {
	m := metrics.New()
	m.RegisterHA()
	m.RegisterHA() // a second call is harmless
	m.HALeader.Set(1)
	m.HAEpoch.Set(42)
	m.HAPromotions.Inc()
	m.HADemotions.WithLabelValues("fenced").Inc()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"sqi_server_ha_leader 1",
		"sqi_server_ha_epoch 42",
		"sqi_server_ha_promotions_total 1",
		`sqi_server_ha_demotions_total{reason="fenced"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("exposition missing %q", want)
		}
	}
}

// TestNew_NoHAMetricsWithoutRegisterHA: a server outside an HA group exposes
// no sqi_server_ha_* series, so a sqi_server_ha_leader == 1 selector never
// matches a single server and its /metrics stays as it was.
func TestNew_NoHAMetricsWithoutRegisterHA(t *testing.T) {
	m := metrics.New()
	m.HALeader.Set(1) // set but unregistered: not exposed
	m.HADemotions.WithLabelValues("fenced").Inc()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "sqi_server_ha_") {
		t.Errorf("exposition carries HA series without RegisterHA:\n%s", rec.Body.String())
	}
}
