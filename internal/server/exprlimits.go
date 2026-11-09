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
// the value internal/openjd enforces.
//
// It is exported and lives here because this package is the only one that may
// import both sides: internal/config must not import internal/openjd (two
// same-level internal packages, and internal/openjd imports internal/store).
// The four fields are same-typed int64s, so a transposed pair would compile
// and enforce the wrong bounds; TestExprLimitsFromConfig gives each field a
// distinct value to catch that.
//
// config.Validate enforces the ranges these values must fall in at startup.
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
// The override is unconditional. The scheduler does not evaluate expressions;
// it uses these limits only to refuse to dispatch an EXPR job to a worker whose
// advertised caps are tighter than the limits the template was accepted under
// (internal/scheduler's exprcaps.go). Comparing against anything other than the
// submitter's own limits would gate on a number no submission used, so both are
// set from one field.
//
// The scheduler's bus-up source is taken from client (nil for none) unless
// the caller's Scheduler section already set one. Heartbeats ride the bus, so
// the liveness sweeps stand down while it is down or has just come back; both
// broker modes need that, since a broker can restart while the server stays
// up.
func schedulerConfig(cfg Config, client *bus.Client) scheduler.Config {
	sched := cfg.Scheduler
	sched.ExprLimits = cfg.OpenJDExprLimits
	sched.NATSAuthEnabled = cfg.NATS.Auth.Enabled
	if sched.BusUpSince == nil && client != nil {
		sched.BusUpSince = client.BusUpSince
	}
	if sched.BusReconnectGrace <= 0 {
		sched.BusReconnectGrace = busReconnectGrace(sched.WorkerTimeout)
	}
	return sched
}

// schedulerConfigFor is [schedulerConfig] for this server, with the
// scheduler's RevokeWorker hook wired to the server's broker: it drops the
// broker connection of a capacity-group member the scheduler has just
// deleted, since a retired worker may still be connected when its
// deregistration is handled. The deletion already revoked the credential in
// the store, in the same transaction, so the hook writes nothing there:
// revoking again by worker ID could revoke a credential enrolled under that
// ID since.
func (s *Server) schedulerConfigFor(client *bus.Client) scheduler.Config {
	sched := schedulerConfig(s.cfg, client)
	sched.RevokeWorker = s.disconnectWorker
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
