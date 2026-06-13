// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metrics "github.com/uberware/sqi/internal/worker/metrics"
)

func TestNew_RegistersAndServes(t *testing.T) {
	m := metrics.New()
	if m == nil {
		t.Fatal("New returned nil")
	}

	// Record one observation on each family so they appear in the exposition.
	m.ActiveTasks.Set(2)
	m.TasksTotal.WithLabelValues("succeeded").Inc()
	m.ExecDuration.WithLabelValues("succeeded").Observe(1.5)
	m.NATSPublishedTotal.WithLabelValues("worker.heartbeat").Inc()
	m.NATSConsumedTotal.WithLabelValues("work.assign.default").Inc()
	m.UpdateUptime()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"sqi_worker_active_tasks",
		"sqi_worker_tasks_total",
		"sqi_worker_exec_duration_seconds",
		"sqi_worker_nats_published_total",
		"sqi_worker_nats_consumed_total",
		"sqi_worker_uptime_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics exposition missing %q", want)
		}
	}
}

func TestNew_IsolatedRegistries(_ *testing.T) {
	// Two instances must not panic on double-registration (private registries).
	_ = metrics.New()
	_ = metrics.New()
}
