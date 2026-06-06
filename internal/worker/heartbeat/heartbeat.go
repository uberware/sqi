// SPDX-License-Identifier: AGPL-3.0-only

// Package heartbeat implements the worker's periodic liveness signal to
// sqi-server.
//
// # Overview
//
// A [Publisher] ticks on a configurable interval and publishes a
// [protocol.HeartbeatMsg] to the [bus.SubjectWorkerHeartbeat] NATS subject.
// Each message carries the worker's current runtime state (active task count,
// active task IDs, uptime, last assignment time) so the server can detect
// stale assignments without additional store queries.
//
// # Watchdog (task 32)
//
// [Publisher.Run] also spawns an internal watchdog goroutine that polls the
// NATS connection status every [watchdogPollInterval]. When it observes a
// transition from a non-connected state back to connected it calls
// [Registrar.Register] — but only if the reconnect callback installed by
// [registration.Registrar.SetupReconnectHook] did not already succeed (as
// indicated by [Registrar.LastRegisteredAt]). This guards against the case
// where the callback's publish fails or is lost without producing a duplicate
// registration on every normal reconnect.
//
// # Latency warning (task 33)
//
// After publishing a heartbeat the Publisher calls [nats.Conn.FlushTimeout]
// to drain the NATS outbound buffer to the TCP layer. If the flush does not
// complete within half the configured interval the Publisher logs a warning,
// indicating that the NATS server or network connection is under strain and
// backpressure is building in the write buffer.
//
// # StateSource
//
// [StateSource] is the interface through which the Publisher reads live task
// state. It is satisfied by the executor (tasks 49+). Until the executor
// exists, callers pass a [NoopStateSource] which reports zero active tasks.
package heartbeat

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// natsConn is the subset of [nats.Conn] used by Publisher. The interface lets
// tests inject a stub without requiring a live NATS server.
// [*nats.Conn] satisfies this interface.
type natsConn interface {
	Publish(subj string, data []byte) error
	FlushTimeout(timeout time.Duration) error
	IsConnected() bool
}

// watchdogPollInterval is how often the watchdog goroutine polls the NATS
// connection status for reconnect transitions. 500 ms gives a worst-case
// detection lag of 500 ms before the watchdog re-registers — well within
// any practical heartbeat interval.
const watchdogPollInterval = 500 * time.Millisecond

// StateSource provides the Publisher with live task-execution state.
// Implemented by the executor package (tasks 49+); use [NoopStateSource]
// until then.
type StateSource interface {
	// ActiveTaskCount returns the number of task processes currently running.
	ActiveTaskCount() int
	// ActiveTaskIDs returns a snapshot of the IDs of all currently running
	// tasks. Implementations must return a value that is safe for the caller
	// to hold across concurrent mutations of the underlying task set — either
	// a snapshot copy or an immutable slice.
	ActiveTaskIDs() []string
	// LastAssignmentAt returns the time of the most recent task assignment,
	// or nil if the worker has not yet received any assignment in this process
	// lifetime.
	LastAssignmentAt() *time.Time
}

// NoopStateSource is a [StateSource] that always reports zero active tasks.
// Use it before the executor is available (tasks 49+).
type NoopStateSource struct{}

// ActiveTaskCount always returns 0.
func (NoopStateSource) ActiveTaskCount() int { return 0 }

// ActiveTaskIDs always returns nil.
func (NoopStateSource) ActiveTaskIDs() []string { return nil }

// LastAssignmentAt always returns nil.
func (NoopStateSource) LastAssignmentAt() *time.Time { return nil }

// Registrar is the subset of [registration.Registrar] used by the watchdog to
// trigger re-registration when a NATS reconnect is detected.
//
// Defined as an interface so that tests can substitute a lightweight stub
// without constructing a full Registrar.
type Registrar interface {
	// Register publishes a registration message to the server. Safe to call
	// multiple times; the server upserts based on worker ID.
	Register(ctx context.Context) error
	// LastRegisteredAt returns the time of the most recent successful Register
	// call, or the zero time if Register has never been called successfully.
	// The watchdog uses this to avoid re-registering when the reconnect callback
	// already handled it.
	LastRegisteredAt() time.Time
}

// Publisher periodically publishes [protocol.HeartbeatMsg] to
// [bus.SubjectWorkerHeartbeat] and runs an internal watchdog goroutine that
// triggers re-registration when the NATS connection is restored after a drop
// and the reconnect callback did not already succeed.
//
// Obtain one with [New] and call [Publisher.Run] in a goroutine; it blocks
// until ctx is canceled.
type Publisher struct {
	nc                 natsConn
	workerID           string
	maxConcurrentTasks int
	interval           time.Duration
	// watchdogInterval is how often runWatchdog polls IsConnected.
	// Defaults to watchdogPollInterval; overridable in tests (same package).
	watchdogInterval time.Duration
	state            StateSource
	registrar        Registrar
	logger           *slog.Logger
	startTime        time.Time
}

// New creates a Publisher. nc and registrar must be non-nil.
//
//   - workerID: the stable worker identity string published in each message.
//   - maxConcurrentTasks: the worker's configured concurrency ceiling,
//     included in each heartbeat for server-side capacity tracking.
//   - interval: how often to publish; must be > 0.
//   - state: provides live task counts and IDs; pass [NoopStateSource] until
//     the executor is wired in.
//   - registrar: called by the watchdog when a reconnect is detected and the
//     callback did not already re-register.
func New(
	nc natsConn,
	workerID string,
	maxConcurrentTasks int,
	interval time.Duration,
	state StateSource,
	registrar Registrar,
	logger *slog.Logger,
) *Publisher {
	return &Publisher{
		nc:                 nc,
		workerID:           workerID,
		maxConcurrentTasks: maxConcurrentTasks,
		interval:           interval,
		watchdogInterval:   watchdogPollInterval,
		state:              state,
		registrar:          registrar,
		logger:             logger,
		startTime:          time.Now(),
	}
}

// Run starts the heartbeat ticker and the NATS watchdog goroutine.
// It blocks until ctx is canceled. Callers should invoke Run in a goroutine.
//
// The watchdog and the ticker share the same parent context; canceling ctx
// stops both cleanly.
func (p *Publisher) Run(ctx context.Context) {
	p.logger.InfoContext(
		ctx, "heartbeat: publisher started",
		slog.String("worker_id", p.workerID),
		slog.Duration("interval", p.interval),
	)

	// Start the watchdog goroutine before the first tick so that a reconnect
	// during the initial startup window is not missed.
	go p.runWatchdog(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.InfoContext(
				context.Background(), "heartbeat: publisher stopped",
				slog.String("worker_id", p.workerID),
			)
			return
		case <-ticker.C:
			p.publish(ctx)
		}
	}
}

// publish builds and sends one HeartbeatMsg. After the fire-and-forget
// Publish call, it flushes the NATS outbound buffer with FlushTimeout (task 33):
// if the TCP send buffer does not drain within half the configured interval,
// backpressure is building and the NATS server or network path is likely under
// strain.
func (p *Publisher) publish(ctx context.Context) {
	now := time.Now()

	msg := protocol.HeartbeatMsg{
		Version:            protocol.ProtocolVersion,
		Type:               protocol.TypeHeartbeat,
		WorkerID:           p.workerID,
		At:                 now,
		ActiveTaskCount:    p.state.ActiveTaskCount(),
		MaxConcurrentTasks: p.maxConcurrentTasks,
		ActiveTaskIDs:      p.state.ActiveTaskIDs(),
		UptimeSeconds:      time.Since(p.startTime).Seconds(),
		LastAssignmentAt:   p.state.LastAssignmentAt(),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		// Marshal of a static struct should never fail; log at error level.
		p.logger.ErrorContext(
			ctx, "heartbeat: marshal failed",
			slog.String("worker_id", p.workerID),
			slog.Any("error", err),
		)
		return
	}

	if err := p.nc.Publish(bus.SubjectWorkerHeartbeat, data); err != nil {
		p.logger.WarnContext(
			ctx, "heartbeat: publish failed",
			slog.String("worker_id", p.workerID),
			slog.Any("error", err),
		)
		return
	}

	// Task 33: flush the outbound write buffer and measure the drain time.
	// nc.Publish is fire-and-forget (it copies the payload into an in-process
	// buffer); FlushTimeout blocks until the OS TCP send buffer accepts the
	// data. A slow or timed-out flush means the network connection or NATS
	// server cannot keep up with the write rate.
	threshold := p.interval / 2
	flushStart := time.Now()
	if err := p.nc.FlushTimeout(threshold); err != nil {
		p.logger.WarnContext(
			ctx, "heartbeat: outbound buffer flush exceeded half the configured interval — NATS server or network may be under strain",
			slog.String("worker_id", p.workerID),
			slog.Duration("flush_elapsed", time.Since(flushStart)),
			slog.Duration("interval", p.interval),
			slog.Duration("threshold", threshold),
			slog.Any("error", err),
		)
		return
	}

	flushElapsed := time.Since(flushStart)
	p.logger.DebugContext(
		ctx, "heartbeat: published",
		slog.String("worker_id", p.workerID),
		slog.Duration("flush_latency", flushElapsed),
		slog.Int("active_tasks", msg.ActiveTaskCount),
		slog.Float64("uptime_seconds", msg.UptimeSeconds),
	)
}

// runWatchdog polls the NATS connection status on [watchdogPollInterval] and
// triggers re-registration when it observes a transition from a non-connected
// state back to connected — but only if the reconnect callback installed by
// registration.Registrar.SetupReconnectHook did not already succeed (task 32).
//
// The coordination mechanism: the watchdog records the time it first observed
// the connection going down (disconnectedAt). On reconnect, it checks whether
// Registrar.LastRegisteredAt() is more recent than disconnectedAt. If so, the
// callback already ran and succeeded; the watchdog skips Register to avoid a
// duplicate publish. If not (callback failed or never ran), the watchdog calls
// Register as a fallback.
func (p *Publisher) runWatchdog(ctx context.Context) {
	ticker := time.NewTicker(p.watchdogInterval)
	defer ticker.Stop()

	// Track connection state so we can detect connected→down→connected cycles.
	wasConnected := p.nc.IsConnected()

	// disconnectedAt records when the watchdog first observed the connection
	// drop. Zero means the connection has been up since the watchdog started.
	var disconnectedAt time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			isConnected := p.nc.IsConnected()

			switch {
			case wasConnected && !isConnected:
				// Connection just dropped. Record the time so we can later tell
				// whether the reconnect callback fired after this moment.
				disconnectedAt = time.Now()
				p.logger.InfoContext(
					ctx, "heartbeat watchdog: NATS connection dropped",
					slog.String("worker_id", p.workerID),
				)

			case !wasConnected && isConnected && !disconnectedAt.IsZero():
				// Connection is back up. Do not re-register if the reconnect
				// callback already handled it (LastRegisteredAt after the drop).
				if p.registrar.LastRegisteredAt().After(disconnectedAt) {
					p.logger.DebugContext(
						ctx, "heartbeat watchdog: reconnect detected — reconnect callback already re-registered, skipping",
						slog.String("worker_id", p.workerID),
					)
				} else {
					// Callback did not fire or its Register call failed — fill in.
					// Skip re-registration if ctx is already done (worker is
					// shutting down and will call Deregister instead).
					if ctx.Err() != nil {
						// Worker is shutting down; Deregister will run instead.
						p.logger.DebugContext(
							ctx, "heartbeat watchdog: reconnect detected but worker shutting down — skipping re-registration",
							slog.String("worker_id", p.workerID),
						)
					} else {
						// ctx is still live (verified above); use it so the
						// re-registration inherits cancellation.
						p.logger.InfoContext(
							ctx, "heartbeat watchdog: reconnect callback did not re-register — triggering fallback re-registration",
							slog.String("worker_id", p.workerID),
						)
						if err := p.registrar.Register(ctx); err != nil {
							p.logger.ErrorContext(
								ctx, "heartbeat watchdog: fallback re-registration failed",
								slog.String("worker_id", p.workerID),
								slog.Any("error", err),
							)
						}
					}
				}
				disconnectedAt = time.Time{} // reset for the next cycle
			}

			wasConnected = isConnected
		}
	}
}
