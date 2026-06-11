// SPDX-License-Identifier: AGPL-3.0-or-later

// Package metrics defines and registers all Prometheus metrics for sqi-worker.
//
// Callers obtain a [*Metrics] via [New] and pass it to the components that
// record observations. A private [prometheus.Registry] is used rather than the
// global default to prevent cross-contamination between test instances.
//
// Metric families defined here:
//
//   - [Metrics.ActiveTasks]           — current in-flight task count
//   - [Metrics.TasksTotal]            — cumulative completions by status
//   - [Metrics.ExecDuration]          — per-task process execution histogram
//   - [Metrics.NATSPublishedTotal]    — NATS publish count by subject
//   - [Metrics.NATSConsumedTotal]     — NATS consume count by subject
//   - [Metrics.UptimeSeconds]         — worker process uptime gauge
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every Prometheus metric family for sqi-worker and the isolated
// registry they are registered against. Obtain one with [New].
type Metrics struct {
	reg       *prometheus.Registry
	startTime time.Time

	// ActiveTasks is the current number of task processes running on this
	// worker. Incremented when a process starts, decremented when it exits.
	ActiveTasks prometheus.Gauge

	// TasksTotal counts task completions, labeled by terminal status:
	// "succeeded", "failed", or "canceled".
	TasksTotal *prometheus.CounterVec

	// ExecDuration records the wall-clock execution time of each task process
	// in seconds, labeled by terminal status so callers can separate fast
	// failures from long-running successes.
	ExecDuration *prometheus.HistogramVec

	// NATSPublishedTotal counts messages published to the sqi-server NATS
	// broker, labeled by subject. Populated by the NATS client in task 20.
	NATSPublishedTotal *prometheus.CounterVec

	// NATSConsumedTotal counts messages consumed from the sqi-server NATS
	// broker, labeled by subject. Populated by the NATS client in task 20.
	NATSConsumedTotal *prometheus.CounterVec

	// UptimeSeconds is the wall-clock seconds since the worker process started.
	// Updated by [Metrics.UpdateUptime].
	UptimeSeconds prometheus.Gauge
}

// New creates a [*Metrics], registers all metric families plus the standard Go
// runtime and process collectors against a fresh [prometheus.Registry], and
// returns it.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		reg:       reg,
		startTime: time.Now(),

		ActiveTasks: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "sqi_worker",
			Name:      "active_tasks",
			Help:      "Current number of task processes actively running on this worker.",
		}),

		TasksTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi_worker",
				Name:      "tasks_total",
				Help:      "Cumulative task completions, partitioned by terminal status (succeeded, failed, canceled).",
			},
			[]string{"status"},
		),

		ExecDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "sqi_worker",
				Name:      "exec_duration_seconds",
				Help:      "Wall-clock execution time of each task process in seconds, partitioned by terminal status.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"status"},
		),

		NATSPublishedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi_worker",
				Subsystem: "nats",
				Name:      "published_total",
				Help:      "Total messages published to the sqi-server NATS broker, partitioned by subject.",
			},
			[]string{"subject"},
		),

		NATSConsumedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "sqi_worker",
				Subsystem: "nats",
				Name:      "consumed_total",
				Help:      "Total messages consumed from the sqi-server NATS broker, partitioned by subject.",
			},
			[]string{"subject"},
		),

		UptimeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "sqi_worker",
			Name:      "uptime_seconds",
			Help:      "Seconds elapsed since the worker process started.",
		}),
	}

	reg.MustRegister(
		m.ActiveTasks,
		m.TasksTotal,
		m.ExecDuration,
		m.NATSPublishedTotal,
		m.NATSConsumedTotal,
		m.UptimeSeconds,
	)

	return m
}

// UpdateUptime sets UptimeSeconds to the elapsed time since the worker started.
// Call this on a ticker in the observability server's goroutine.
func (m *Metrics) UpdateUptime() {
	m.UptimeSeconds.Set(time.Since(m.startTime).Seconds())
}

// Handler returns an [http.Handler] that serves the Prometheus text exposition
// format (and OpenMetrics when the client requests it) for all metrics in this
// [*Metrics]' private registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}
