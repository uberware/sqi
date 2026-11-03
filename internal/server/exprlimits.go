// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ExprLimitsFromConfig maps the operator's four openjd.expr_* settings onto
// the value internal/openjd actually enforces.
//
// It lives here, and is exported, because this package is the only one that
// may see both sides. internal/config must not import internal/openjd
// (a cross-import between two same-level internal packages, and internal/openjd
// pulls in internal/store), so the mapping cannot live next to either
// definition -- and left inline in cmd/sqi-server's Config literal it would be
// four assignments between four same-typed int64 fields with nothing testing
// that they are paired correctly. Transposing two of them would compile,
// start, and enforce the wrong bounds. TestExprLimitsFromConfig gives each
// field a distinct value to catch that.
//
// The ranges these values must fall in are enforced by config.Validate at
// startup, not here: by the time this runs the configuration has already been
// accepted or the process has already exited.
func ExprLimitsFromConfig(c config.OpenJDConfig) openjd.ExprLimits {
	return openjd.ExprLimits{
		SubmissionOperations:  c.ExprOperationLimit,
		SubmissionMemoryBytes: c.ExprMemoryLimit,
		TemplatePositions:     c.ExprTemplatePositions,
		TemplateRetainedBytes: c.ExprTemplateRetainedBytes,
	}
}

// schedulerConfig returns the [scheduler.Config] the server runs its scheduler
// with: the caller's Scheduler section, with the EXPR limits overwritten from
// [Config.OpenJDExprLimits].
//
// The override is deliberate and unconditional. The scheduler does not evaluate
// expressions; it uses these limits for exactly one thing — refusing to
// dispatch an EXPR job to a worker whose advertised caps are tighter than the
// limits the template was ACCEPTED under (internal/scheduler's exprcaps.go).
// Comparing against anything other than the submitter's own limits would gate
// on a number no submission ever used, so the two are set from one field rather
// than left to a caller to keep in step.
func schedulerConfig(cfg Config) scheduler.Config {
	sched := cfg.Scheduler
	sched.ExprLimits = cfg.OpenJDExprLimits
	sched.NATSAuthEnabled = cfg.NATS.Auth.Enabled
	return sched
}

// schedulerConfigWithBus is [schedulerConfig] with the scheduler's bus-up
// source taken from client unless the caller's Scheduler section already set
// one. Heartbeats ride the bus, so the liveness sweeps stand down while it is
// down or has just come back; both broker modes need that, since a broker can
// restart while the server stays up.
func schedulerConfigWithBus(cfg Config, client *bus.Client) scheduler.Config {
	sched := schedulerConfig(cfg)
	if sched.BusUpSince == nil && client != nil {
		sched.BusUpSince = client.BusUpSince
	}
	if sched.BusReconnectGrace <= 0 {
		sched.BusReconnectGrace = busReconnectGrace(sched.WorkerTimeout)
	}
	return sched
}

// busReconnectGrace is how long after the server's bus connection returns the
// liveness sweeps keep standing down. A live worker lost the same broker, so
// it redials on its own backoff, which is independent of the server's: the
// grace must cover the worker timeout, a worker's longest redial wait with its
// jitter, and one heartbeat interval for the first heartbeat after the redial.
// A shorter grace reclaims the running tasks of a worker that is merely still
// waiting to reconnect.
func busReconnectGrace(workerTimeout time.Duration) time.Duration {
	if workerTimeout <= 0 {
		workerTimeout = scheduler.DefaultConfig().WorkerTimeout
	}
	redial := time.Duration(float64(protocol.MaxReconnectBackoff) * (1 + protocol.ReconnectJitterFraction))
	return workerTimeout + redial + protocol.DefaultHeartbeatInterval
}
