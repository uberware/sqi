// SPDX-License-Identifier: AGPL-3.0-only

// Package metrics defines and registers all Prometheus metrics for sqi-server.
//
// Callers obtain a [*Metrics] via [New] and pass it to the components that
// record observations. Using a private [prometheus.Registry] rather than the
// global default registry prevents cross-contamination between test instances
// and avoids collision with Go runtime metrics.
//
// Many of the metric families defined here are stubs whose values are set to
// zero until the corresponding component lands:
//
//   - [Metrics.SchedulerQueueDepth], [Metrics.SchedulerTasksTotal] — populated
//     by the scheduler in tasks 46–55.
//   - [Metrics.WorkersTotal] — populated by the worker registry in task 47.
//   - [Metrics.NATSPublishedTotal], [Metrics.NATSConsumedTotal] — populated by
//     the typed NATS client wrapper in task 36.
//   - [Metrics.DBQueryDuration] — populated by the SQLite store in task 29.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every Prometheus metric family for sqi-server and the isolated
// registry they are registered against. Obtain one with [New].
type Metrics struct {
	reg *prometheus.Registry

	// ── HTTP ──────────────────────────────────────────────────────────────────

	// HTTPRequestsTotal counts completed HTTP requests, labeled by HTTP
	// method, request path, and response status code.
	//
	// TODO(task 66): switch the "path" label to chi's route pattern
	// (chi.RouteContext(r.Context()).RoutePattern()) to avoid high cardinality
	// from parameterised segments such as /api/v1/jobs/{id}.
	HTTPRequestsTotal *prometheus.CounterVec

	// HTTPRequestDuration records request latency in seconds, labeled by HTTP
	// method and request path.
	HTTPRequestDuration *prometheus.HistogramVec

	// ── Scheduler ─────────────────────────────────────────────────────────────

	// SchedulerQueueDepth is the current number of ready tasks waiting for
	// assignment in each named queue. Set by the scheduler (tasks 46–55).
	SchedulerQueueDepth *prometheus.GaugeVec

	// SchedulerTasksTotal counts task completions, labeled by queue name and
	// terminal status (succeeded, failed, canceled). Incremented by the
	// scheduler (tasks 46–55).
	SchedulerTasksTotal *prometheus.CounterVec

	// ── Workers ───────────────────────────────────────────────────────────────

	// WorkersTotal is the current number of registered workers, partitioned by
	// status: online, offline, or disabled. Set by the worker registry
	// (task 47).
	WorkersTotal *prometheus.GaugeVec

	// ── NATS JetStream ────────────────────────────────────────────────────────

	// NATSPublishedTotal counts messages published to the embedded NATS broker,
	// labeled by subject. Incremented by the typed bus client (task 36).
	NATSPublishedTotal *prometheus.CounterVec

	// NATSConsumedTotal counts messages consumed from the embedded NATS broker,
	// labeled by subject. Incremented by the typed bus client (task 36).
	NATSConsumedTotal *prometheus.CounterVec

	// ── SQLite store ──────────────────────────────────────────────────────────

	// DBQueryDuration records SQLite query execution time in seconds, labeled
	// by operation name (e.g. "job.insert", "task.list"). Observed by the store
	// implementation (task 29).
	DBQueryDuration *prometheus.HistogramVec
}

// New creates a [*Metrics] and registers all metric families — plus the
// standard Go runtime and process collectors — against a fresh
// [prometheus.Registry].
func New() *Metrics {
	reg := prometheus.NewRegistry()

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		reg: reg,

		HTTPRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "http",
				Name:      "requests_total",
				Help:      "Total HTTP requests completed, partitioned by method, path, and status code.",
			},
			[]string{"method", "path", "status_code"},
		),

		HTTPRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "sqi",
				Subsystem: "http",
				Name:      "request_duration_seconds",
				Help:      "HTTP request latency in seconds, partitioned by method and path.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"method", "path"},
		),

		SchedulerQueueDepth: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "scheduler",
				Name:      "queue_depth",
				Help:      "Current number of ready tasks waiting for assignment, partitioned by queue name.",
			},
			[]string{"queue"},
		),

		SchedulerTasksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "scheduler",
				Name:      "tasks_total",
				Help:      "Cumulative task completions, partitioned by queue name and terminal status.",
			},
			[]string{"queue", "status"},
		),

		WorkersTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "workers",
				Name:      "total",
				Help:      "Current number of registered workers, partitioned by status (online, offline, disabled).",
			},
			[]string{"status"},
		),

		NATSPublishedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "nats",
				Name:      "published_total",
				Help:      "Total messages published to the embedded NATS broker, partitioned by subject.",
			},
			[]string{"subject"},
		),

		NATSConsumedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "nats",
				Name:      "consumed_total",
				Help:      "Total messages consumed from the embedded NATS broker, partitioned by subject.",
			},
			[]string{"subject"},
		),

		DBQueryDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "sqi",
				Subsystem: "db",
				Name:      "query_duration_seconds",
				Help:      "SQLite query execution time in seconds, partitioned by operation name.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"operation"},
		),
	}

	reg.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.SchedulerQueueDepth,
		m.SchedulerTasksTotal,
		m.WorkersTotal,
		m.NATSPublishedTotal,
		m.NATSConsumedTotal,
		m.DBQueryDuration,
	)

	return m
}

// Handler returns an [http.Handler] that serves the Prometheus text exposition
// format (and OpenMetrics when the client requests it) for all metrics
// registered in this [*Metrics]' private registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}
