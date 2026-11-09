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

// The capacity families serve nothing until a group's series is set, so a
// farm with no capacity group exposes no sqi_capacity_* lines at all, and
// their names are pinned once set.
func TestNew_CapacityMetrics(t *testing.T) {
	m := metrics.New()
	serve := func() string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	if body := serve(); strings.Contains(body, "sqi_capacity_") {
		t.Errorf("exposition carries capacity series before any is set:\n%s", body)
	}

	m.CapacityDemandCores.WithLabelValues("burst").Set(8)
	m.CapacityDesiredMachines.WithLabelValues("burst").Set(2)
	m.CapacityMembers.WithLabelValues("burst", "online").Set(1)
	body := serve()
	for _, want := range []string{
		`sqi_capacity_demand_cores{group="burst"} 8`,
		`sqi_capacity_desired_machines{group="burst"} 2`,
		`sqi_capacity_members{group="burst",state="online"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("exposition missing %q", want)
		}
	}
}

// TestNew_NoHAMetricsWithoutRegisterHA: a server outside an HA group exposes
// no sqi_server_ha_* series, so a sqi_server_ha_leader == 1 selector never
// matches a single server.
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
