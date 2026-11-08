// SPDX-License-Identifier: AGPL-3.0-or-later

// Package metrics defines and registers all Prometheus metrics for sqi-server.
//
// Callers obtain a [*Metrics] via [New] and pass it to the components that
// record observations. Using a private [prometheus.Registry] rather than the
// global default registry prevents cross-contamination between test instances
// and avoids collision with Go runtime metrics.
//
// [Metrics.SchedulerTasksTotal], [Metrics.SchedulerAssignmentDuration],
// [Metrics.NATSPublishedTotal], [Metrics.NATSConsumedTotal] and
// [Metrics.DBQueryDuration] are registered, but no server code records into
// them, so they serve no samples. The other families are written by the
// scheduler and the other components named on their fields.
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every Prometheus metric family for sqi-server and the isolated
// registry they are registered against. Obtain one with [New].
type Metrics struct {
	reg *prometheus.Registry

	haOnce sync.Once // registers the HA families once

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
	// terminal status (succeeded, failed, canceled). Nothing records it.
	SchedulerTasksTotal *prometheus.CounterVec

	// SchedulerTasksReconciled counts attempts reclaimed because their worker
	// stopped reporting them (scheduler.reconcile_grace).
	SchedulerTasksReconciled prometheus.Counter

	// SchedulerCancelsResent counts the cancels the heartbeat sweep sends to a
	// worker whose heartbeat still lists an attempt the store has closed
	// (cause "closed") or has no record of (cause "unknown").
	SchedulerCancelsResent *prometheus.CounterVec

	// SchedulerAssignmentDuration records the wall-clock time spent in a
	// single call to tryAssign, labeled by result:
	//   - "assigned"  — a worker was found and the task was dispatched.
	//   - "deferred"  — no eligible worker or policy blocked; task stays ready.
	//   - "error"     — an unexpected error aborted the attempt.
	//
	// Nothing records it.
	SchedulerAssignmentDuration *prometheus.HistogramVec

	// SchedulerIdleWorkers is the current count of online workers that have no
	// task in 'assigned' or 'running' state, partitioned by farm ID.
	// Updated by the scheduler after each worker-registry or sweep event.
	SchedulerIdleWorkers *prometheus.GaugeVec

	// ── Workers ───────────────────────────────────────────────────────────────

	// WorkersTotal is the current number of registered workers, partitioned by
	// status: online, offline, or disabled. Set by the scheduler.
	WorkersTotal *prometheus.GaugeVec

	// ── NATS JetStream ────────────────────────────────────────────────────────

	// NATSPublishedTotal counts messages published to the NATS broker,
	// labeled by subject. Nothing records it.
	NATSPublishedTotal *prometheus.CounterVec

	// NATSConsumedTotal counts messages consumed from the NATS broker,
	// labeled by subject. Nothing records it.
	NATSConsumedTotal *prometheus.CounterVec

	// ── SQLite store ──────────────────────────────────────────────────────────

	// DBQueryDuration records SQLite query execution time in seconds, labeled
	// by operation name (e.g. "job.insert", "task.list"). Nothing records it.
	DBQueryDuration *prometheus.HistogramVec

	// ── Usage pools ───────────────────────────────────────────────────────────

	// UsageActiveClaims is the current number of active (unreleased)
	// usage-pool claims for each named pool. Updated by the scheduler on every
	// dispatch tick.
	UsageActiveClaims *prometheus.GaugeVec

	// ── Auto-retry ────────────────────────────────────────────────────────────

	// TaskRetriesTotal counts tasks re-queued by automatic retry, partitioned
	// by queue name. Incremented by the scheduler's failure fork.
	TaskRetriesTotal *prometheus.CounterVec

	// JobsAutoParkedTotal counts jobs auto-parked at their failure limit,
	// partitioned by queue name. Incremented by the scheduler's failure fork.
	JobsAutoParkedTotal *prometheus.CounterVec

	// ── Capacity groups ───────────────────────────────────────────────────────
	// Set by the scheduler's capacity sweep, one series per enabled group,
	// labeled by group name. With no enabled group none of them has a series.

	// CapacityDemandCores is the cores of ready work assigned to each group:
	// work one of its machines could run that no idle worker and no
	// higher-priority group took.
	CapacityDemandCores *prometheus.GaugeVec

	// CapacityDesiredMachines is the total number of machines each group
	// should have, counting the machines it already has.
	CapacityDesiredMachines *prometheus.GaugeVec

	// CapacityMembers counts each group's members by liveness (state online
	// or offline).
	CapacityMembers *prometheus.GaugeVec

	// ── High availability ─────────────────────────────────────────────────────
	// Created by New but exposed only once [Metrics.RegisterHA] is called, so a
	// server outside a group serves no sqi_server_ha_* series.

	// HALeader is 1 while this server leads its high-availability group and 0
	// otherwise, including on a server outside a group. Every server of a
	// group is scraped; the leader is the one reporting 1.
	HALeader prometheus.Gauge

	// HAEpoch is the leadership epoch: the one this server leads at, or on a
	// standby the one it last saw the leader hold.
	HAEpoch prometheus.Gauge

	// HAPromotions counts this process's promotions to leader: at most one,
	// since a demoted leader exits.
	HAPromotions prometheus.Counter

	// HADemotions counts losses of leadership, partitioned by reason
	// (renew_refused, deadline, fenced, guard_lost, broker_lost,
	// promotion_failed). The process exits after one, so the series is
	// mostly read from its last scrape and the exit code.
	HADemotions *prometheus.CounterVec
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
				Help:      "Current number of ready tasks waiting for assignment, partitioned by queue ID. Excludes tasks in retry backoff, tasks on a paused queue, and tasks under a paused, parked or terminal job.",
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

		SchedulerTasksReconciled: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "scheduler",
				Name:      "tasks_reconciled_total",
				Help:      "Attempts reclaimed because their live worker no longer reported them.",
			},
		),

		SchedulerCancelsResent: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "scheduler",
				Name:      "cancels_resent_total",
				Help:      "Cancels sent to a live worker still reporting an attempt the store has closed or has no record of, by cause (closed, unknown).",
			},
			[]string{"cause"},
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
				Help:      "Total messages published to the NATS broker, partitioned by subject.",
			},
			[]string{"subject"},
		),

		NATSConsumedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "nats",
				Name:      "consumed_total",
				Help:      "Total messages consumed from the NATS broker, partitioned by subject.",
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

		UsageActiveClaims: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "usage",
				Name:      "active_claims",
				Help:      "Current number of active (unreleased) usage-pool claims per pool name.",
			},
			[]string{"pool"},
		),

		TaskRetriesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "scheduler",
				Name:      "task_retries_total",
				Help:      "Total tasks re-queued by automatic retry, partitioned by queue name.",
			},
			[]string{"queue"},
		),

		JobsAutoParkedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "scheduler",
				Name:      "jobs_autoparked_total",
				Help:      "Total jobs auto-parked at their failure limit, partitioned by queue name.",
			},
			[]string{"queue"},
		),

		CapacityDemandCores: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "capacity",
				Name:      "demand_cores",
				Help:      "Cores of ready work assigned to the capacity group: work one of its machines could run that no idle worker and no higher-priority group took.",
			},
			[]string{"group"},
		),

		CapacityDesiredMachines: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "capacity",
				Name:      "desired_machines",
				Help:      "Total machines the capacity group should have, counting the machines it already has (not the number to add).",
			},
			[]string{"group"},
		),

		CapacityMembers: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "capacity",
				Name:      "members",
				Help:      "Members of the capacity group, partitioned by state (online, offline).",
			},
			[]string{"group", "state"},
		),

		HALeader: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "server_ha",
				Name:      "leader",
				Help:      "1 while this server leads its high-availability group, 0 otherwise.",
			},
		),

		HAEpoch: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "sqi",
				Subsystem: "server_ha",
				Name:      "epoch",
				Help:      "The leadership epoch this server leads at, or last saw the leader hold.",
			},
		),

		HAPromotions: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "server_ha",
				Name:      "promotions_total",
				Help:      "Promotions of this server to leader of its high-availability group.",
			},
		),

		HADemotions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi",
				Subsystem: "server_ha",
				Name:      "demotions_total",
				Help:      "Losses of leadership by this server, partitioned by reason.",
			},
			[]string{"reason"},
		),
	}

	reg.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.SchedulerQueueDepth,
		m.SchedulerTasksTotal,
		m.SchedulerTasksReconciled,
		m.SchedulerCancelsResent,
		m.SchedulerAssignmentDuration,
		m.SchedulerIdleWorkers,
		m.WorkersTotal,
		m.NATSPublishedTotal,
		m.NATSConsumedTotal,
		m.DBQueryDuration,
		m.UsageActiveClaims,
		m.TaskRetriesTotal,
		m.JobsAutoParkedTotal,
		m.CapacityDemandCores,
		m.CapacityDesiredMachines,
		m.CapacityMembers,
	)

	// Both causes are exported from the start, at 0, so a rate over them is
	// defined before the first resend.
	for _, cause := range []string{"closed", "unknown"} {
		m.SchedulerCancelsResent.WithLabelValues(cause)
	}

	return m
}

// RegisterHA exposes the sqi_server_ha_* families. A server in a
// high-availability group calls it once at start; further calls do nothing.
// Without it the families can be written to but are not served, so the
// /metrics of a server outside a group is unchanged and a
// sqi_server_ha_leader == 1 selector never matches it.
func (m *Metrics) RegisterHA() {
	m.haOnce.Do(func() {
		m.reg.MustRegister(m.HALeader, m.HAEpoch, m.HAPromotions, m.HADemotions)
	})
}

// Handler returns an [http.Handler] that serves the Prometheus text exposition
// format (and OpenMetrics when the client requests it) for all metrics
// registered in this [*Metrics]' private registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}
