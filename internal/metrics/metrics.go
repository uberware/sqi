// SPDX-License-Identifier: AGPL-3.0-or-later

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
//   - [Metrics.SchedulerQueueDepth], [Metrics.SchedulerTasksTotal],
//     [Metrics.SchedulerAssignmentDuration], [Metrics.SchedulerIdleWorkers] —
//     populated by the scheduler.
//   - [Metrics.WorkersTotal] — populated by the worker registry.
//   - [Metrics.NATSPublishedTotal], [Metrics.NATSConsumedTotal] — populated by
//     the typed NATS client wrapper.
//   - [Metrics.DBQueryDuration] — populated by the SQLite store.
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
	// TODO: switch the "path" label to chi's route pattern
	// (chi.RouteContext(r.Context()).RoutePattern()) to avoid high cardinality
	// from parameterised segments such as /api/v1/jobs/{id}.
	HTTPRequestsTotal *prometheus.CounterVec

	// HTTPRequestDuration records request latency in seconds, labeled by HTTP
	// method and request path.
	HTTPRequestDuration *prometheus.HistogramVec

	// ── Scheduler ─────────────────────────────────────────────────────────────

	// SchedulerQueueDepth is the current number of ready tasks waiting for
	// assignment in each named queue. Set by the scheduler.
	SchedulerQueueDepth *prometheus.GaugeVec

	// SchedulerTasksTotal counts task completions, labeled by queue name and
	// terminal status (succeeded, failed, canceled). Incremented by the
	// scheduler.
	SchedulerTasksTotal *prometheus.CounterVec

	// SchedulerAssignmentDuration records the wall-clock time spent in a
	// single call to tryAssign, labeled by result:
	//   - "assigned"  — a worker was found and the task was dispatched.
	//   - "deferred"  — no eligible worker or policy blocked; task stays ready.
	//   - "error"     — an unexpected error aborted the attempt.
	//
	// Populated by the scheduler assignment loop.
	SchedulerAssignmentDuration *prometheus.HistogramVec

	// SchedulerIdleWorkers is the current count of online workers that have no
	// task in 'assigned' or 'running' state, partitioned by farm ID.
	// Updated by the scheduler after each worker-registry or sweep event.
	SchedulerIdleWorkers *prometheus.GaugeVec

	// ── Workers ───────────────────────────────────────────────────────────────

	// WorkersTotal is the current number of registered workers, partitioned by
	// status: online, offline, or disabled. Set by the worker registry.
	WorkersTotal *prometheus.GaugeVec

	// ── NATS JetStream ────────────────────────────────────────────────────────

	// NATSPublishedTotal counts messages published to the embedded NATS broker,
	// labeled by subject. Incremented by the typed bus client.
	NATSPublishedTotal *prometheus.CounterVec

	// NATSConsumedTotal counts messages consumed from the embedded NATS broker,
	// labeled by subject. Incremented by the typed bus client.
	NATSConsumedTotal *prometheus.CounterVec

	// ── SQLite store ──────────────────────────────────────────────────────────

	// DBQueryDuration records SQLite query execution time in seconds, labeled
	// by operation name (e.g. "job.insert", "task.list"). Observed by the store
	// implementation.
	DBQueryDuration *prometheus.HistogramVec

	// ── License pools ─────────────────────────────────────────────────────────

	// LicenseActiveCheckouts is the current number of active (unreleased)
	// license checkouts for each named pool. Updated by the scheduler on every
	// dispatch tick.
	LicenseActiveCheckouts *prometheus.GaugeVec
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

		SchedulerAssignmentDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "sqi",
				Subsystem: "scheduler",
				Name:      "assignment_duration_seconds",
				Help:      "Wall-clock time for a single task-assignment attempt, partitioned by result (assigned, deferred, error).",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"result"},
		),

		SchedulerIdleWorkers: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "scheduler",
				Name:      "idle_workers",
				Help:      "Current number of online workers with no active (assigned or running) task, partitioned by farm ID.",
			},
			[]string{"farm"},
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

		LicenseActiveCheckouts: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "licenses",
				Name:      "active_checkouts",
				Help:      "Current number of active (unreleased) license checkouts per pool name.",
			},
			[]string{"pool"},
		),
	}

	reg.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.SchedulerQueueDepth,
		m.SchedulerTasksTotal,
		m.SchedulerAssignmentDuration,
		m.SchedulerIdleWorkers,
		m.WorkersTotal,
		m.NATSPublishedTotal,
		m.NATSConsumedTotal,
		m.DBQueryDuration,
		m.LicenseActiveCheckouts,
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
