// SPDX-License-Identifier: AGPL-3.0-or-later

// Package scheduler implements the sqi-server assignment loop and worker
// registry. It decides which task runs on which worker.
//
// The [Scheduler] runs three concurrent loops:
//
//  1. Lease subscriber: a core-NATS request/reply subscriber on work.lease.>
//     ([handleLeaseRequest]). Idle workers ask for work; the scheduler selects a
//     priority-ordered batch of ready tasks the worker is eligible for that fits
//     its free CPU cores ([selectLeaseBatch]), atomically leases each
//     ([store.TaskStore.LeaseTask]), and replies with the assignment
//     payloads. When no work is available the request parks in the waiter
//     registry until new work appears or the hold elapses, then replies.
//
//  2. Worker registry: a NATS push-consumer for worker.register.<worker> messages that
//     persists capability data via [store.WorkerStore.RegisterWorker] and keeps
//     the WorkersTotal Prometheus gauge current.
//
//  3. Heartbeat sweep: a NATS push-consumer that updates each worker's
//     LastHeartbeatAt on worker.heartbeat messages, paired with a periodic
//     timer that finds workers whose heartbeat has gone stale
//     ([store.WorkerStore.ListStaleWorkers], a candidate list only) and takes
//     each offline with [store.WorkerStore.OfflineStaleWorker], which re-checks
//     the heartbeat inside its write, closes the worker's open attempts,
//     releases their usage claims and returns its in-flight tasks to the ready
//     queue in one transaction.
//     The same tick refreshes the queue-depth, idle-worker, and usage-claim
//     Prometheus gauges.
//
// Worker selection. A task is matched to a worker by capability tags,
// compute-location affinity, and queue/farm filtering ([WorkerEligible]),
// subject to per-queue and per-farm maximum-concurrent-task limits
// ([policyGate]). Those checks are early filters. Once a worker is chosen,
// [store.TaskStore.LeaseTask] makes the real decision in one transaction: it
// moves the task to assigned, re-checks the queue, farm and usage-pool caps
// against their current values, creates the provisional [store.TaskAttempt] and
// claims any required usage pool slots. If a cap filled in the meantime nothing
// is written and the task stays ready for the next lease request. Attempt numbers
// are computed by the same transaction — 1 for a fresh task, N+1 on retry.
//
// Assignment payload. [buildAssignPayload] re-parses the job's raw OpenJD
// template to extract the matching step's OnRun action, embedded files, and
// ordered environments (job environments first, then step, per the OpenJD
// spec). It also attaches path-mapping rules built from the registered storage
// locations for the worker's compute location.
//
// Status and log ingestion. A push-consumer on the SQI_TASK stream
// ([handleTaskStatusMessage]) decodes [protocol.TaskStatusMsg] from
// task.status.<worker>.<job>, updates the task/attempt, releases held usage pool slots, and
// drives step/job completion including [openjd.ResolveDependencies] for
// multi-step jobs. A push-consumer on SQI_LOGS ([handleLogChunk]) persists each
// task.logs.<worker>.<task> chunk as a [store.TaskLog] row, recording both the
// worker-assigned sequence number and the NATS stream sequence that serves as
// the log-tail pagination cursor.
//
// Cancellation. [CancelJob] and [CancelTask] are the server-side entry points
// called by the REST layer: each makes one store call that transitions tasks to
// [store.TaskStatusCanceled], closes their running attempts and releases their
// usage pool slots in a single transaction, then publishes a cancel signal to
// each worker that held one, on its task.cancel.<workerID> subject
// ([bus.Client.PublishTaskCancel]). The logic lives in cancellation.go; the
// SQI_CANCEL stream and publish helper live in the bus package.
//
// Wire protocol and metrics. All worker messages use the versioned JSON types
// in [worker/protocol] ([protocol.RegisterMsg], [protocol.HeartbeatMsg],
// [protocol.AssignMsg], [protocol.TaskStatusMsg], [protocol.LogChunkMsg]). Each
// heartbeat-sweep tick refreshes the scheduler's Prometheus metrics
// ([metrics.Metrics]): queue depth by queue, idle workers by farm, and active
// usage-pool claims per pool.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/capacity/script"
	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/wol"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// DefaultConfig returns a [Config] with the default values.
func DefaultConfig() Config {
	return Config{
		FarmID:                    "",
		AssignInterval:            time.Second,
		AssignBatchSize:           50,
		AssignWorkers:             4,
		WorkerTimeout:             30 * time.Second,
		HeartbeatSweepInterval:    15 * time.Second,
		AssignedTaskTimeout:       30 * time.Second,
		OfflineWorkerRetention:    24 * time.Hour,
		JobRetention:              7 * 24 * time.Hour,
		JobRetentionIncludeFailed: false,
		UnschedulableGrace:        30 * time.Second,
		ReconcileGrace:            2 * time.Minute,
		DefaultMaxAttempts:        3,
		RetryDelay:                30 * time.Second,
		DefaultFailureLimit:       0,
		ExprLimits:                openjd.DefaultExprLimits(),
	}
}

// Config holds tuning parameters for the [Scheduler].
type Config struct {
	// FarmID is the farm this scheduler instance manages. The assignment loop
	// only considers tasks that belong to this farm.
	FarmID string

	// AssignInterval is how often the assignment loop polls the store for ready
	// tasks. Lower values reduce latency; higher values reduce DB pressure.
	// Default: 1 s.
	AssignInterval time.Duration

	// AssignBatchSize is the maximum number of ready tasks fetched per
	// assignment loop tick. Limits burst DB load without capping throughput
	// over time. Default: 50.
	AssignBatchSize int

	// AssignWorkers is the size of the goroutine pool that processes assignment
	// decisions concurrently. Default: 4.
	AssignWorkers int

	// WorkerTimeout is the maximum time since a worker's last heartbeat before
	// the heartbeat sweep considers it offline. Default: 30 s.
	WorkerTimeout time.Duration

	// BusUpSince reports when the message bus connection was last established
	// or re-established, and the zero time while it is down. Worker heartbeats
	// and running-reports travel over that bus, so while it is down (or has
	// just come back) their absence says nothing about the workers: the
	// liveness sweeps decide nothing until the bus has been up for
	// BusReconnectGrace. Nil means no grace: every sweep acts on what the
	// store holds. Not a tuning knob; the server wires it from its bus client.
	BusUpSince func() time.Time

	// BusReconnectGrace is how long the bus must have been up before the
	// liveness sweeps act on heartbeats again. It must cover the time a live
	// worker needs to notice the outage ended, redial and heartbeat, which is
	// longer than WorkerTimeout when workers back off between redials. A value
	// <= 0 is coerced to WorkerTimeout in [New]; the server sets a larger
	// value derived from the workers' reconnect behavior. Unused when
	// BusUpSince is nil.
	BusReconnectGrace time.Duration

	// HeartbeatSweepInterval is how often the heartbeat sweep runs. Should be
	// well below WorkerTimeout so stale workers are caught promptly. Default: 15 s.
	HeartbeatSweepInterval time.Duration

	// AssignedTaskTimeout is the maximum time a task may sit in 'assigned'
	// without transitioning to 'running' before the reaper returns it to the
	// ready queue. It guards the brief leased→running window: a task a worker
	// leased but never reported running (e.g. the worker crashed between lease
	// and process start). The heartbeat sweep cannot recover such tasks while the
	// worker is still heartbeating, so this reaper runs independently on the
	// heartbeat sweep tick. Default: 30 s.
	AssignedTaskTimeout time.Duration

	// OfflineWorkerRetention is how long a worker may remain in
	// [store.WorkerStatusOffline] before the retention sweep hard-deletes it,
	// bounding the growth of the worker table on farms with ephemeral nodes.
	// Disabled and online workers are never auto-removed. A value <= 0 disables
	// the sweep entirely. Default: 24 h.
	OfflineWorkerRetention time.Duration

	// JobRetention is how long a terminal job is kept before the retention
	// sweep hard-deletes it and all of its data (steps, tasks, attempts, logs).
	// completed and canceled jobs are always eligible; failed jobs only when
	// JobRetentionIncludeFailed is set. A value <= 0 disables the sweep.
	// Default: 168 h (7 days).
	JobRetention time.Duration

	// JobRetentionIncludeFailed extends the retention sweep to failed jobs.
	// Default: false (failed jobs are kept for debugging).
	JobRetentionIncludeFailed bool

	// UnschedulableGrace is how long a ready task may wait with no eligible
	// online worker before it is flagged unschedulable (surfaced in the
	// API/UI). A value <= 0 disables the sweep entirely. Unlike the other
	// duration knobs above, it is not coerced up to the default in [New], since
	// 0 is a legitimate "off" setting. Default: 30 s.
	UnschedulableGrace time.Duration

	// ReconcileGrace is how long a live worker may go without reporting an
	// attempt it holds before the sweep reclaims that attempt (see
	// attemptreconcile.go). 0 disables reconciliation; like
	// UnschedulableGrace, [New] does not coerce it up to the default, but it
	// raises a positive value below WorkerTimeout to WorkerTimeout. Reports
	// are trusted only when the worker's timestamp is within WorkerTimeout of
	// their receipt. Default: 2 m.
	ReconcileGrace time.Duration

	// DefaultMaxAttempts is the server-level fallback tier of the layered
	// retry policy (Server -> Farm -> Queue -> Job): the farm-wide default
	// number of attempts a task may make before going terminal-failed. A
	// value <= 0 is coerced up to the [DefaultConfig] value in [New], since
	// unlike UnschedulableGrace this knob has no meaningful "off" state (the
	// minimum valid value is 1, which disables auto-retry outright).
	// Default: 3.
	DefaultMaxAttempts int

	// RetryDelay is the server-level fallback default backoff before a
	// failed task re-enters the ready queue. 0 is a legitimate "immediate"
	// setting; only a negative value is coerced up to the [DefaultConfig]
	// value in [New]. Default: 30 s.
	RetryDelay time.Duration

	// DefaultFailureLimit is the server-level fallback default job-level
	// failure ceiling. 0 = off (no auto-park) and is a legitimate setting —
	// like UnschedulableGrace, it is not coerced up in [New]. Default: 0.
	DefaultFailureLimit int

	// ExprLimits are the OpenJD EXPR limits this server enforces when it
	// accepts a template (internal/config's openjd.expr_* keys). The scheduler
	// does not evaluate expressions; it uses them to compare against each
	// worker's advertised caps before dispatching an EXPR job (see
	// exprcaps.go). It must be the same value the submitter is built with, so
	// internal/server sets both from one field.
	//
	// Zero fields normalize to the defaults in [New].
	ExprLimits openjd.ExprLimits

	// NATSAuthEnabled mirrors config.NATSAuthConfig.Enabled (server.Config's
	// NATSAuthEnabled). It gates two things here: whether worker registration
	// touches the worker's broker-credential LastSeenAt, and where a worker's
	// capacity-group membership comes from (its credential when on, its
	// worker.capacity_group setting when off; see capacitymembership.go).
	// With broker authentication off there are no credential rows, and
	// registration does no extra store work (see handleWorkerRegister).
	NATSAuthEnabled bool

	// RevokeWorker drops a removed ephemeral capacity-group member's broker
	// connection. The scheduler calls it after the store has deleted the
	// worker, which revoked its credential in the same transaction, so an
	// implementation must not revoke again by worker ID: that could revoke a
	// credential enrolled under the same ID since. It runs on a goroutine of
	// its own, because dropping a broker connection takes seconds. Nil means
	// the store's revocation alone applies. Not a tuning knob; the server
	// wires it.
	RevokeWorker func(ctx context.Context, workerID string) error

	// ScriptProviders are the capacity provider programs from the server's
	// configuration file, by name without the "script:" prefix. A group
	// naming one that is not here does not act. Not a tuning knob; the
	// server wires it.
	ScriptProviders map[string]script.Provider

	// WakeOnLANPort is the UDP port magic packets go to. A value <= 0 is
	// coerced to [wol.DefaultPort] in [New].
	WakeOnLANPort int

	// WakeOnLANSendTo are extra "ip:port" endpoints the server sends magic
	// packets to when it sends them itself.
	WakeOnLANSendTo []string

	// SendWakePackets sends magic packets for macs to each of targets
	// ("ip:port") and reports how many targets it reached, with an error
	// joining every failure. Nil means [wol.Send] with its default burst and
	// gap.
	SendWakePackets func(ctx context.Context, macs []net.HardwareAddr, targets []string) (int, error)

	// OnFenced is called once, with the error, the first time a store write
	// made under the context [Scheduler.Run] was given fails with
	// [store.ErrFenced]: another server has taken the leadership, so this one
	// must stop. It runs on a goroutine of its own and may call
	// [Scheduler.Stop] and wait for Run to return. By then the scheduler has
	// already stopped acknowledging messages and leasing tasks (see
	// fence.go). Nil means the loss is only logged. Not a tuning knob; the
	// server wires it when scheduler high availability is on.
	OnFenced func(error)
}

// busClient is the subset of [bus.Client] used by the Scheduler. Defined as
// an interface so unit tests can inject a stub without a real NATS broker.
type busClient interface {
	ConsumeWorker(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error)
	ConsumeTaskStatus(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error)
	ConsumeTaskLogs(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error)
	PublishTaskCancel(ctx context.Context, workerID string, data []byte) error
	SubscribeWorkerDiag(handler func(subject string, data []byte)) (*nats.Subscription, error)
	SubscribeLease(handler func(workerID, queueID string, data []byte) []byte) (*nats.Subscription, error)
	RequestWorkerControl(ctx context.Context, workerID string, data []byte) ([]byte, error)
}

// Scheduler owns the assignment loop, worker registry, and heartbeat sweep.
// Create it with [New] and drive it with [Run].
type Scheduler struct {
	cfg      Config
	store    store.Store
	bus      busClient
	metrics  *metrics.Metrics
	logger   *slog.Logger
	notifier ws.Notifier // pushes live events to WebSocket clients

	// queueDepthSeen holds every queue ID [Scheduler.refreshQueueDepthGauge]
	// has set the SchedulerQueueDepth gauge for. The store reports only queues
	// that still have ready tasks, so this is how a queue that has drained is
	// found and reset. Touched only by refreshQueueDepthGauge, which runs on
	// the heartbeat-sweep goroutine alone, so it needs no lock.
	queueDepthSeen map[string]struct{}

	// diagBuf is the in-memory diagnostic-log ring buffer fed by the
	// worker.diag.> core-NATS subscriber. Nil when diagnostics are disabled.
	diagBuf *diag.Buffer

	// diagSub is the core-NATS subscription for worker.diag.> messages,
	// unsubscribed during shutdown. Nil when diagnostics are disabled.
	diagSub *nats.Subscription

	// leaseSub is the core-NATS subscription for worker lease requests,
	// unsubscribed during shutdown.
	leaseSub *nats.Subscription

	// waiters parks long-poll lease requests per queue; woken by wake triggers.
	waiters *waiterRegistry

	// disabledWaiters parks a disabled worker's lease requests, keyed by worker
	// ID; [Scheduler.WakeWorker] wakes them when the worker is enabled.
	disabledWaiters *waiterRegistry

	// attemptCache holds recently-seen task-attempt ownership (workerID,
	// taskID), consulted by handleLogChunk before it reads the store. See
	// [attemptOwnerCache].
	attemptCache *attemptOwnerCache

	// leaseLocks serializes lease selection per worker. Concurrent lease
	// requests for the same worker (one outstanding request per queue it
	// serves, plus retry overlap) must not both read the same committed-core
	// count and each lease up to free, over-committing the worker. The lock is
	// held only around selectLeaseBatch, never across the long-poll park.
	leaseLocks sync.Map // workerID -> *sync.Mutex

	// exprCapWarned de-duplicates the registration-time EXPR-limit warning:
	// workerID -> the last shortfall text logged for it. See
	// [Scheduler.warnOnExprCapShortfall]. Entries are cleared when a worker
	// stops being short but not when it is deleted, so the map retains one
	// short string per distinct worker ID that has registered short with this
	// process. It is not pruned and starts empty on restart.
	exprCapWarned sync.Map // workerID -> string

	// capWarned de-duplicates capacity-group membership warnings, which a
	// worker would otherwise repeat on every reconnect: capWarnKey -> the
	// detail last logged. See [Scheduler.warnCapacityOnce]. Bounded like
	// exprCapWarned: a few entries per worker that ever drew one.
	capWarned sync.Map // capWarnKey -> string

	// instanceRefusals counts, per worker process, the lease requests refused
	// in a row because that process's registration has not landed, so one
	// that never lands is reported at Warn (see
	// [Scheduler.noteInstanceRefusal]). An entry is dropped when that
	// process's request is next served. A process that is refused and then
	// gone for good leaves its entry until this server restarts: one small
	// record per such process.
	instanceRefusals sync.Map // instanceRefusalKey -> *instanceRefusal

	// instanceRefusalWarnAfter and instanceRefusalWarnEvery set when that Warn
	// fires: once a process has been refused this many times in a row, then at
	// most once per interval while it goes on. Overridable in tests.
	instanceRefusalWarnAfter int
	instanceRefusalWarnEvery time.Duration

	// leaseHoldTimeout bounds how long an unfulfillable lease request parks
	// before replying empty. Overridable in tests.
	leaseHoldTimeout time.Duration

	// leaseRefusalDelay is how long a lease request from a worker process
	// whose registration has not landed is held before its empty reply (see
	// [Scheduler.leaseFromUnregisteredInstance]). The worker re-requests as
	// soon as a reply arrives, so answering at once would make its lease loop
	// spin. It is short because the registration normally lands within it; a
	// disabled worker's request is held for leaseHoldTimeout instead. Overridable
	// in tests.
	leaseRefusalDelay time.Duration

	// wg tracks all internal goroutines so [Run] can wait for clean exit.
	wg sync.WaitGroup

	// revokes runs the [Config.RevokeWorker] calls in the background (see
	// [Scheduler.revokeRemovedWorker]). Run closes it at shutdown and waits,
	// for at most [revokeDrainTimeout], for the calls still running.
	revokes backgroundCalls

	// ctx is the lifecycle context for the scheduler; set during [Run] and used
	// by NATS message-handler callbacks that cannot receive a context parameter.
	// After Run returns ctx is canceled.
	ctx context.Context

	// cancel is called to stop all internal goroutines; set during [Run].
	cancel context.CancelFunc

	// retryWakeMu guards retryWakeTimers: the in-flight backoff-wake timers
	// scheduled by scheduleRetryWake, tracked so shutdown can stop them
	// instead of leaving fire-and-forget timers alive past [Run].
	retryWakeMu     sync.Mutex
	retryWakeTimers map[*time.Timer]struct{}

	// reconciler remembers which attempts each live worker's heartbeats
	// report, so the sweep can reclaim one a worker stopped reporting. Nil
	// when Config.ReconcileGrace is 0 (reconciliation disabled).
	reconciler *reconciler

	// startedAt is when [Run] started, which under HA is when this server
	// became the leader. Reconciliation holds off for the first ReconcileGrace
	// after it, while live heartbeats rebuild the reconciler, and the liveness
	// sweeps count their bus grace from it when the bus came up earlier (see
	// [Scheduler.livenessDeferred]).
	startedAt time.Time

	// deferLog remembers which liveness deferral was last logged, so each is
	// logged once rather than on every sweep tick.
	deferLog deferralLog

	// fence records a fenced store write; see fence.go.
	fence fenceState

	// capState is what the capacity sweep keeps between ticks; see
	// capacity.go.
	capState capacityState

	// diagMu guards diagStarted, which is set once the worker.diag
	// subscription is running, by [Scheduler.StartDiag] or by Run.
	diagMu      sync.Mutex
	diagStarted bool
}

// deferralLog is the state behind [Scheduler.logLivenessDeferred].
type deferralLog struct {
	mu sync.Mutex
	// logged is set once a deferral has been logged; key is the bus-up time
	// it was logged for, zero for an outage.
	logged bool
	key    time.Time
	// firstUp is the first bus-up time the scheduler saw: the connection
	// made at startup.
	firstUp time.Time
}

// New creates a Scheduler. Call [Run] to start its goroutines.
//
// notifier receives live-event notifications after each state change so the
// WebSocket hub can fan them out to subscribed clients. Pass
// [ws.NoopNotifier] (or nil, which is treated as NoopNotifier) when no
// WebSocket hub is wired.
//
// diagBuf, when non-nil, enables worker diagnostic-log ingestion: the scheduler
// subscribes to worker.diag.> and appends records under "worker:<id>". Pass nil
// to disable diagnostics.
func New(cfg Config, st store.Store, busClient busClient, m *metrics.Metrics, logger *slog.Logger, notifier ws.Notifier, diagBuf *diag.Buffer) *Scheduler {
	if cfg.AssignBatchSize <= 0 {
		cfg.AssignBatchSize = DefaultConfig().AssignBatchSize
	}
	if cfg.AssignWorkers <= 0 {
		cfg.AssignWorkers = DefaultConfig().AssignWorkers
	}
	if cfg.AssignInterval <= 0 {
		cfg.AssignInterval = DefaultConfig().AssignInterval
	}
	if cfg.WorkerTimeout <= 0 {
		cfg.WorkerTimeout = DefaultConfig().WorkerTimeout
	}
	if cfg.HeartbeatSweepInterval <= 0 {
		cfg.HeartbeatSweepInterval = DefaultConfig().HeartbeatSweepInterval
	}
	if cfg.AssignedTaskTimeout <= 0 {
		cfg.AssignedTaskTimeout = DefaultConfig().AssignedTaskTimeout
	}
	if cfg.DefaultMaxAttempts <= 0 {
		cfg.DefaultMaxAttempts = DefaultConfig().DefaultMaxAttempts
	}
	if cfg.RetryDelay < 0 {
		cfg.RetryDelay = DefaultConfig().RetryDelay
	}
	// Normalized here so exprCapShortfall compares against the values a
	// submission is metered under. An unset field left at 0 would report "no
	// shortfall" in that dimension for every worker.
	cfg.ExprLimits = cfg.ExprLimits.Normalized()
	if cfg.BusReconnectGrace <= 0 {
		cfg.BusReconnectGrace = cfg.WorkerTimeout
	}
	if cfg.WakeOnLANPort <= 0 {
		cfg.WakeOnLANPort = wol.DefaultPort
	}
	if cfg.SendWakePackets == nil {
		cfg.SendWakePackets = wol.SendDefault
	}
	n := notifier
	if n == nil {
		n = ws.NoopNotifier{}
	}
	var rec *reconciler
	if cfg.ReconcileGrace > 0 {
		// The skew bound is the heartbeat timeout, and the reconciler's
		// pruning needs grace >= skew. Config validation enforces that for
		// scheduler.reconcile_grace; a Config built directly has its grace
		// raised to WorkerTimeout here. 0 stays 0: disabled.
		cfg.ReconcileGrace = max(cfg.ReconcileGrace, cfg.WorkerTimeout)
		rec = newReconciler(cfg.ReconcileGrace, cfg.WorkerTimeout)
	}
	return &Scheduler{
		cfg:               cfg,
		store:             st,
		bus:               busClient,
		metrics:           m,
		logger:            logger,
		notifier:          n,
		diagBuf:           diagBuf,
		waiters:           newWaiterRegistry(),
		disabledWaiters:   newWaiterRegistry(),
		attemptCache:      newAttemptOwnerCache(),
		leaseHoldTimeout:  30 * time.Second,
		leaseRefusalDelay: time.Second,
		// About half a minute of refusals at leaseRefusalDelay on one queue:
		// far longer than a registration takes to land.
		instanceRefusalWarnAfter: 30,
		instanceRefusalWarnEvery: 5 * time.Minute,
		retryWakeTimers:          make(map[*time.Timer]struct{}),
		reconciler:               rec,
		// ctx is replaced with the derived cancellable context in Run. The
		// background fallback keeps NATS callbacks from dereferencing a nil
		// context if invoked before Run (e.g. in a partial test setup).
		ctx: context.Background(),
	}
}

// Run starts all scheduler goroutines and blocks until ctx is canceled.
// It returns after all goroutines have exited cleanly.
//
// ctx is the context every handler, sweep and lease writes under. Under
// scheduler high availability it carries the leader's epoch
// ([store.WithLeaderEpoch]), so a write made after another server took over
// fails with [store.ErrFenced] (see fence.go).
func (s *Scheduler) Run(ctx context.Context) error {
	s.startedAt = time.Now().UTC()
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.ctx = ctx
	defer cancel()

	s.logger.InfoContext(
		ctx, "scheduler: starting",
		slog.String("farm_id", s.cfg.FarmID),
		slog.Duration("assign_interval", s.cfg.AssignInterval),
		slog.Int("assign_workers", s.cfg.AssignWorkers),
		slog.Duration("worker_timeout", s.cfg.WorkerTimeout),
		slog.Duration("heartbeat_sweep_interval", s.cfg.HeartbeatSweepInterval),
		slog.Duration("offline_worker_retention", s.cfg.OfflineWorkerRetention),
		slog.Duration("job_retention", s.cfg.JobRetention),
		slog.Bool("job_retention_include_failed", s.cfg.JobRetentionIncludeFailed),
		slog.Duration("unschedulable_grace", s.cfg.UnschedulableGrace),
		slog.Duration("reconcile_grace", s.cfg.ReconcileGrace),
	)

	// ── Worker NATS consumer ────────────────────────────────
	// A single JetStream push-consumer delivers the worker register,
	// heartbeat and deregister messages. The handler dispatches by subject.
	_, err := s.bus.ConsumeWorker(ctx, s.handleWorkerMessage)
	if err != nil {
		return fmt.Errorf("scheduler: start worker consumer: %w", err)
	}
	s.logger.InfoContext(ctx, "scheduler: worker consumer started")

	// ── Task-status NATS consumer ────────────────────────────────
	// A JetStream push-consumer on SQI_TASK delivers task.status.<worker>.<job>
	// messages from workers. handleTaskStatusMessage updates the store,
	// closes attempt records, releases usage pool slots, and drives step/job
	// completion.
	if err := s.startTaskStatusConsumer(ctx); err != nil {
		return fmt.Errorf("scheduler: start task-status consumer: %w", err)
	}
	s.logger.InfoContext(ctx, "scheduler: task-status consumer started")

	// ── Task-logs NATS consumer ──────────────────────────────────
	// A JetStream push-consumer on SQI_LOGS delivers task.logs.<worker>.<task>
	// messages from workers. handleLogChunk persists each chunk to the
	// task_logs table with NATS sequence as the pagination cursor.
	if err := s.startTaskLogsConsumer(ctx); err != nil {
		return fmt.Errorf("scheduler: start task-logs consumer: %w", err)
	}
	s.logger.InfoContext(ctx, "scheduler: task-logs consumer started")

	// ── Worker diagnostic-log subscriber ─────────────────────────
	// A core-NATS subscriber on worker.diag.> decodes worker diagnostic
	// records into the in-memory ring buffer. No-op when diagnostics are
	// disabled (diagBuf nil), or when StartDiag already started it.
	if err := s.startDiag(ctx); err != nil {
		return err
	}

	// ── Stuck-step repair ─────────────────────────────────────────
	// Finalize, once, the steps earlier releases left stuck (see
	// reconcileStuckSteps). Then release pending steps a retry reset but never
	// released (reconcilePendingSteps). Both run before the lease subscriber
	// starts, so no work is leased while the repair is still releasing or
	// canceling steps, and before the sweeps begin. The task-status consumer is
	// already running, so a report arriving concurrently is safe: every write on
	// the completion path is guarded and idempotent.
	s.reconcileStuckSteps(ctx)
	s.reconcilePendingSteps(ctx)

	// ── Lease subscriber ──────────────────────────────────────────
	// A core-NATS request-reply subscriber that handles worker lease requests.
	// Workers ask for work; handleLeaseRequest selects a batch or parks until
	// new work appears (long-poll) and then replies.
	leaseSub, err := s.bus.SubscribeLease(s.handleLeaseRequest)
	if err != nil {
		return fmt.Errorf("scheduler: start lease subscriber: %w", err)
	}
	s.leaseSub = leaseSub
	s.logger.InfoContext(ctx, "scheduler: lease subscriber started")

	// ── Heartbeat sweep ───────────────────────────────────────────
	s.wg.Go(func() {
		s.runHeartbeatSweep(ctx)
	})

	// Seed WorkersTotal gauge with current DB state so it is non-zero on
	// startup even if no NATS registration messages have been seen yet.
	s.refreshWorkerGauge(ctx)

	// Block until canceled, then wait for goroutines.
	<-ctx.Done()
	s.logger.InfoContext(ctx, "scheduler: shutdown signaled — draining goroutines")

	// Unsubscribe the core-NATS diagnostic-log subscriber if active.
	s.diagMu.Lock()
	diagSub := s.diagSub
	s.diagMu.Unlock()
	if diagSub != nil {
		if err := diagSub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			s.logger.WarnContext(ctx, "scheduler: diagnostic-log unsubscribe failed", slog.Any("error", err))
		}
	}

	// Unsubscribe the core-NATS lease subscriber if active.
	if s.leaseSub != nil {
		if err := s.leaseSub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			s.logger.WarnContext(ctx, "scheduler: lease subscriber unsubscribe failed", slog.Any("error", err))
		}
	}

	s.wg.Wait()

	// Stop any backoff-wake timers still pending so they don't outlive Run.
	s.stopRetryWakeTimers()

	// Capacity provider calls first: one that finishes a retirement starts a
	// broker revocation, which the wait below then covers.
	s.closeCapacityCalls(ctx)

	// Let broker revocations of removed workers finish, briefly, so none
	// outlives the broker the server closes next.
	if !s.revokes.Close(revokeDrainTimeout) {
		s.logger.WarnContext(ctx, "scheduler: a removed worker's broker revocation was still running at shutdown",
			slog.Duration("waited", revokeDrainTimeout))
	}

	s.logger.InfoContext(ctx, "scheduler: stopped")
	return nil
}

// Stop cancels the scheduler's internal context, triggering a clean shutdown.
// It does not wait for goroutines to finish — that happens inside [Run].
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// reconcileStuckSteps finalizes, once at start, every step of a live
// (non-terminal) job whose tasks are all terminal but which was never
// finalized, and which no future task report will finalize. Such a step is
// left when completion was decided from one page of a step's tasks (a step
// with more than [store.MaxLimit] tasks), or when a single-task cancel hit a
// job's last open task. Nothing reports on those tasks again, so without this
// pass the step, its job, the steps behind it and the jobs blocked on it stay
// stuck.
//
// The repair is not a data migration. Finalizing a step has downstream effects
// (dependency propagation keyed on step names, the transitive cancel cascade,
// job finalization, cross-job dependents and WebSocket events), and doing that
// in SQL would be a second copy of the completion logic that could drift from
// the first and could not emit events. Each step goes through
// [Scheduler.checkStepCompletion], the path a task report takes.
//
// It is idempotent: every write on that path is guarded, and on a farm with no
// such steps the one [store.StepStore.ListStuckSteps] query returns nothing and
// nothing is written. Steps of jobs that are already terminal are ignored,
// because a job cancel finalizes its own steps. A step that fails is logged and
// skipped so one bad row cannot block the rest, and it stays stuck, so the next
// start retries it. Cross-job dependents of a job this pass finalizes are
// reconciled by the same completion path, with [Scheduler.sweepBlockedJobs] as
// its backstop.
func (s *Scheduler) reconcileStuckSteps(ctx context.Context) {
	steps, err := s.store.ListStuckSteps(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: list stuck steps failed", slog.Any("error", err))
		}
		return
	}
	for _, step := range steps {
		if ctx.Err() != nil || s.deposed() {
			return // shutting down: whatever is left is picked up by the next start
		}
		if err := s.checkStepCompletion(ctx, step.ID, step.JobID); err != nil {
			if s.stopIfFenced(ctx, err) {
				return
			}
			if !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(ctx, "scheduler: reconcile stuck step failed",
					slog.String("step_id", step.ID), slog.String("job_id", step.JobID), slog.Any("error", err))
			}
			continue
		}
		s.logger.InfoContext(ctx, "scheduler: reconciled a step of a live job whose tasks were all terminal but never finalized",
			slog.String("step_id", step.ID), slog.String("job_id", step.JobID))
	}
}

// reconcilePendingSteps re-runs dependency resolution, once at start, for every
// live, non-blocked job that has a pending step. A retry commits
// its revived tasks and the step reset before ResolveDependencies runs, so a
// server that stops in between leaves a pending step nothing would release.
// Release and cascade are idempotent and write only what is releasable or can
// never be satisfied, and the job is finalized only when no step of it is left
// open, so a healthy job (a step waiting on a running upstream) is read and
// left alone: a start on a healthy farm makes no completion-path write. A job
// that fails is logged and skipped; the next start retries it.
func (s *Scheduler) reconcilePendingSteps(ctx context.Context) {
	ids, err := s.store.ListJobIDsWithPendingSteps(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: list jobs with pending steps failed", slog.Any("error", err))
		}
		return
	}
	for _, jobID := range ids {
		if ctx.Err() != nil || s.deposed() {
			return // shutting down: whatever is left is picked up by the next start
		}
		s.reconcilePendingJob(ctx, jobID)
	}
}

// reconcilePendingJob releases what is releasable and cascade-cancels what can
// never run in one job, then finalizes the job if that left every step
// terminal. Releasing runs [Scheduler.propagateStepDependencies] as a completed
// upstream would (ResolveDependencies); the cascade runs it as a failed one
// would (CancelDependents). Calling both releases what is releasable and
// cancels what never can be satisfied.
func (s *Scheduler) reconcilePendingJob(ctx context.Context, jobID string) {
	if err := s.propagateStepDependencies(ctx, jobID, store.StepStatusCompleted); err != nil {
		if !s.stopIfFenced(ctx, err) {
			s.logger.WarnContext(ctx, "scheduler: reconcile pending steps: release failed",
				slog.String("job_id", jobID), slog.Any("error", err))
		}
		return
	}
	if err := s.propagateStepDependencies(ctx, jobID, store.StepStatusFailed); err != nil {
		if !s.stopIfFenced(ctx, err) {
			s.logger.WarnContext(ctx, "scheduler: reconcile pending steps: cascade failed",
				slog.String("job_id", jobID), slog.Any("error", err))
		}
		return
	}
	// Only a cascade can leave a job with every step terminal here, and
	// FinalizeJob is a write transaction even when it changes nothing, so it is
	// asked only once a read shows nothing is left open.
	open, err := s.jobHasOpenStep(ctx, jobID)
	if err != nil {
		s.logger.WarnContext(ctx, "scheduler: reconcile pending steps: list steps failed",
			slog.String("job_id", jobID), slog.Any("error", err))
		return
	}
	if open {
		return
	}
	if err := s.checkJobCompletion(ctx, jobID); err != nil && !s.stopIfFenced(ctx, err) {
		s.logger.WarnContext(ctx, "scheduler: reconcile pending steps: job completion failed",
			slog.String("job_id", jobID), slog.Any("error", err))
	}
}

// jobHasOpenStep reports whether any step of the job is not yet terminal. It is
// a gate, not a decision: [store.JobStore.FinalizeJob] still computes the
// outcome inside its own write (invariant I4).
func (s *Scheduler) jobHasOpenStep(ctx context.Context, jobID string) (bool, error) {
	steps, err := s.store.ListSteps(ctx, jobID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(steps, func(st store.Step) bool { return !st.Status.IsTerminal() }), nil
}

// buildUsageClaims converts the step's usage pool requirements into
// [store.UsagePoolClaim] values for [store.LeaseRequest.Claims].
// Each claim gets a fresh UUID as its claim ID.
// Pools not found in the pools map are skipped (the matcher already rejected
// workers when the pool was missing, so this path is unreachable in practice).
func buildUsageClaims(step store.Step, pools map[string]store.UsagePool) []store.UsagePoolClaim {
	if step.HostRequirements == nil || len(step.HostRequirements.UsagePools) == 0 {
		return nil
	}
	claims := make([]store.UsagePoolClaim, 0, len(step.HostRequirements.UsagePools))
	for _, name := range step.HostRequirements.UsagePools {
		pool, ok := pools[strings.ToLower(name)] // pools keyed by lowercased name
		if !ok {
			continue
		}
		claims = append(claims, store.UsagePoolClaim{
			ClaimID:  uuid.NewString(),
			PoolID:   pool.ID,
			PoolName: pool.Name,
		})
	}
	return claims
}

// buildUsageContext fetches all configured usage pools and the current
// active claim count for each pool required by step.
// Both return values are safe to pass to [WorkerEligible] when step has no
// usage pool requirements.
func (s *Scheduler) buildUsageContext(
	ctx context.Context,
	step store.Step,
) (pools map[string]store.UsagePool, activeCounts map[string]int, err error) {
	pools = make(map[string]store.UsagePool)
	activeCounts = make(map[string]int)

	if step.HostRequirements == nil || len(step.HostRequirements.UsagePools) == 0 {
		return pools, activeCounts, nil
	}

	// Fetch all pools once and key them by lowercased name. Usage-pool names are
	// the trailing segment of a case-insensitive capability name (OpenJD
	// jobtemplate-2023-09), so the returned maps are keyed case-insensitively and
	// every consumer ([checkUsagePools], [buildUsageClaims]) looks up by
	// strings.ToLower(name).
	allPools, err := s.store.ListUsagePools(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list usage pools: %w", err)
	}
	for _, p := range allPools {
		pools[strings.ToLower(p.Name)] = p
	}

	// Fetch active claim count only for pools the step actually requires.
	for _, name := range step.HostRequirements.UsagePools {
		lk := strings.ToLower(name)
		pool, ok := pools[lk]
		if !ok {
			// Pool not found; leave activeCounts[lk] as zero so the matcher
			// rejects due to missing pool (capacity = 0).
			continue
		}
		count, err := s.store.ActiveClaimCount(ctx, pool.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("active claim count for pool %q: %w", name, err)
		}
		activeCounts[lk] = count
	}

	return pools, activeCounts, nil
}

// buildAssignPayload is implemented in assign.go.

// ── Worker NATS consumer ─────────────────────────────────────────────

// handleWorkerMessage is the JetStream message handler for the worker
// register, heartbeat and deregister subjects (all three flow through the
// SQI_WORKER stream and its single durable consumer).
//
// Each subject carries the publishing worker's ID as its last token, so the
// routing below recovers that identity before dispatching to the per-message
// handler.
func (s *Scheduler) handleWorkerMessage(msg jetstream.Msg) {
	ctx := s.ctx
	subject := msg.Subject()

	workerID, _, ok := bus.ParseWorkerSubject(subject)
	if !ok {
		s.discardUnexpectedSubject(ctx, msg, "worker")
		return
	}

	switch {
	case strings.HasPrefix(subject, bus.SubjectWorkerRegisterPrefix+"."):
		s.handleWorkerRegister(ctx, msg, workerID)
	case strings.HasPrefix(subject, bus.SubjectWorkerHeartbeatPrefix+"."):
		s.handleWorkerHeartbeat(ctx, msg, workerID)
	case strings.HasPrefix(subject, bus.SubjectWorkerDeregisterPrefix+"."):
		s.handleWorkerDeregister(ctx, msg, workerID)
	default:
		// ParseWorkerSubject also accepts the four-token
		// task.status/task.logs/work.lease shapes, so a subject filter on
		// SQI_WORKER widened to one of those reaches here. A three-token
		// worker.* subject with an unrecognized prefix does not:
		// ParseWorkerSubject accepts only the three it knows, so the !ok
		// branch above handles it.
		s.discardUnexpectedSubject(ctx, msg, "worker")
	}
}

// discardOnVersionMismatch reports whether msg carries a wire-protocol version
// this server does not speak and, if so, logs it and acks the message away.
//
// encoding/json drops every field the receiving struct does not declare, so a
// message that merely decodes is not evidence that the sender and this server
// agree on its shape: a worker built against a different revision hands over a
// half-understood payload and the server would act on it as though nothing
// were missing. The worker applies the mirror-image check to the assignments
// it receives ([protocol.ProtocolVersion],
// internal/worker/lease.decodeAssignment).
//
// Rejection is by exact equality, including the empty string: a payload with
// no envelope comes from a sender this server cannot identify.
//
// The message is acked, never nacked. A version does not change on redelivery,
// so nacking would wedge the consumer replaying one unreadable message. A
// worker whose registration is discarded is absent from the store, and
// [Scheduler.handleLeaseRequest] replies empty for a worker it cannot look up,
// so it is offered no work at all. A worker that registered before this server
// started, and whose heartbeats are now discarded, goes stale and is retired by
// the heartbeat sweep, which reclaims its in-flight tasks, so no explicit
// mark-offline path is needed here.
//
// level is the caller's, because the three channels differ in rate: a
// registration happens once per connect and reconnect, and a task status once
// per in-flight task, but a heartbeat repeats every worker.heartbeat_interval
// (15s by default) for as long as the mismatched worker runs. Logging that one
// at warn level would evict every other server record from the bounded
// diagnostics ring buffer (internal/diag), so the heartbeat call site passes
// debug and relies on the registration warning. A worker always emits that
// warning: a protocol version changes only with a new server build, and
// restarting the server drops every worker's NATS connection, whose reconnect
// hook re-registers.
func (s *Scheduler) discardOnVersionMismatch(
	ctx context.Context, msg jetstream.Msg, level slog.Level, version, workerID, impact string,
) bool {
	if version == protocol.ProtocolVersion {
		return false
	}
	s.logger.Log(
		ctx, level, "scheduler: discarding worker message — wire protocol version mismatch",
		slog.String("subject", msg.Subject()),
		slog.String("worker_id", workerID),
		slog.String("message_version", version),
		slog.String("server_version", protocol.ProtocolVersion),
		slog.String("impact", impact),
		slog.String("remediation", "upgrade the worker to match this server (server first, then workers)"),
	)
	s.ackMsg(ctx, msg)
	return true
}

// discardUnexpectedSubject logs a "<noun> on unexpected subject —
// discarding" warning naming msg's subject and acks it away. noun identifies
// the calling consumer (e.g. "worker", "task.status", "task.logs") in the log
// line. Every consumer uses it for a message whose subject does not parse or
// parses into a shape its dispatch does not recognize. Such a subject cannot
// become valid on redelivery, so this always acks, never naks.
func (s *Scheduler) discardUnexpectedSubject(ctx context.Context, msg jetstream.Msg, noun string) {
	s.logger.WarnContext(
		ctx, "scheduler: "+noun+" on unexpected subject — discarding",
		slog.String("subject", msg.Subject()),
	)
	s.ackMsg(ctx, msg)
}

// discardOnIdentityMismatch acks and discards msg if payloadWorkerID (from
// the decoded message body) disagrees with subjectWorkerID (from the NATS
// subject). The subject is the only identity NATS itself can enforce, so a
// payload claiming a different worker is a permanent failure: redelivery
// cannot make a forged or stale identity legal (see
// [Scheduler.discardOnVersionMismatch]).
//
// An empty payloadWorkerID fails the comparison too, because subjectWorkerID is
// always non-empty ([bus.ParseWorkerSubject] guarantees it).
func (s *Scheduler) discardOnIdentityMismatch(ctx context.Context, msg jetstream.Msg, subjectWorkerID, payloadWorkerID string) bool {
	if payloadWorkerID == subjectWorkerID {
		return false
	}
	s.logger.WarnContext(
		ctx, "scheduler: worker message whose payload identity differs from its subject — discarding",
		slog.String("subject_worker_id", subjectWorkerID),
		slog.String("payload_worker_id", payloadWorkerID),
	)
	s.ackMsg(ctx, msg)
	return true
}

// handleWorkerRegister processes a worker.register message:
// decodes the payload, upserts the worker in the store, reports any tasks the
// store reclaimed because the worker restarted (a new instance ID),
// and refreshes the WorkersTotal Prometheus gauge.
//
// subjectWorkerID is the worker the message's subject attributes it to.
func (s *Scheduler) handleWorkerRegister(ctx context.Context, msg jetstream.Msg, subjectWorkerID string) {
	var m protocol.RegisterMsg
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		// The subject is the only identity left once the body will not decode.
		s.logger.WarnContext(
			ctx, "scheduler: malformed worker.register message",
			slog.String("subject_worker_id", subjectWorkerID),
			slog.Any("error", err),
		)
		s.ackMsg(ctx, msg) // ack to discard; re-delivery cannot fix a bad payload
		return
	}
	if s.discardOnVersionMismatch(ctx, msg, slog.LevelWarn, m.Version, m.WorkerID,
		"this worker is not registered and will be offered no work at all") {
		return
	}
	if s.discardOnIdentityMismatch(ctx, msg, subjectWorkerID, m.WorkerID) {
		return
	}

	// The two struct conversions below (GPUInfo, ExprLimits) are
	// compile-checked on field name, type and declaration order, so drift
	// between the wire type and the store type is a build failure. The
	// top-level copy is by hand;
	// TestHandleWorkerRegister_EveryWireFieldReachesTheStore and its field
	// counts guard it.
	now := time.Now().UTC()
	w := store.Worker{
		ID:              m.WorkerID,
		InstanceID:      m.InstanceID,
		FarmID:          m.FarmID,
		QueueID:         m.QueueID,
		Name:            m.Name,
		Hostname:        m.Hostname,
		IPAddress:       m.IPAddress,
		ComputeLocation: m.ComputeLocation,
		OS:              m.OS,
		OSVersion:       m.OSVersion,
		Arch:            m.Arch,
		Version:         m.WorkerVersion,
		CPUCount:        m.CPUCount,
		RAMMb:           m.RAMMb,
		GPUInfo:         store.GPUInfo(m.GPUInfo),
		Tags:            m.Tags,
		ExprLimits:      store.WorkerExprLimits(m.ExprLimits),
		Extensions:      slices.Clone(m.Extensions),
		Interfaces:      storeInterfaces(m.Interfaces),
		CloudInstanceID: m.CloudInstanceID,
		Status:          store.WorkerStatusOnline,
		LastHeartbeatAt: &now,
	}
	w.RegistrationBrokerAt = s.brokerAt(ctx, msg, "worker.register", m.WorkerID)

	// Touches the broker credential when authentication is on, and reads
	// what capacity-group membership needs (capacitymembership.go).
	rc := s.beforeRegistration(ctx, m, now)

	stored, reg, err := s.store.RegisterWorker(ctx, w)
	if errors.Is(err, store.ErrStaleRegistration) {
		// A registration the broker stamped before the one already recorded: a
		// late redelivery (JetStream can return an older message after a
		// failover). Redelivery cannot make it current, so ack it rather than
		// Nak.
		s.logger.InfoContext(
			ctx, "scheduler: stale registration — discarding",
			slog.String("worker_id", m.WorkerID),
			slog.String("instance_id", m.InstanceID),
			slog.Any("broker_at", w.RegistrationBrokerAt),
			slog.String("stored_instance_id", stored.InstanceID),
			slog.Any("stored_broker_at", stored.RegistrationBrokerAt),
		)
		s.ackMsg(ctx, msg)
		return
	}
	if s.stopIfFenced(ctx, err) {
		return
	}
	if err != nil {
		s.logger.ErrorContext(
			ctx, "scheduler: persist worker registration failed",
			slog.String("worker_id", m.WorkerID),
			slog.Any("error", err),
		)
		s.nakMsg(ctx, msg)
		return
	}
	if len(reg.Reclaimed) > 0 {
		// A new worker process: the previous one's in-flight tasks went back to
		// ready inside the registration write.
		s.reclaimOfflineWorkerTasks(ctx, m.WorkerID, m.Hostname, reclaimRestart, reg.Reclaimed)
	}

	s.ensureComputeLocation(ctx, m.ComputeLocation)
	s.afterRegistration(ctx, m, stored, rc, now)
	s.noteMemberWoke(ctx, reg, stored, now)
	s.warnOnExprCapShortfall(ctx, w)

	s.logger.InfoContext(
		ctx, "scheduler: worker registered",
		slog.String("worker_id", m.WorkerID),
		slog.String("hostname", m.Hostname),
		slog.String("os", m.OS),
		slog.String("farm_id", m.FarmID),
	)
	s.notifier.NotifyWorker(ws.WorkerEvent{
		WorkerID: m.WorkerID,
		Name:     m.Name,
		Hostname: m.Hostname,
		FarmID:   m.FarmID,
		// The stored row's effective status, which is not always the online
		// this registration asked for (a disabled worker stays disabled).
		Status: string(stored.EffectiveStatus()),
	})
	s.refreshWorkerGauge(ctx)
	s.ackMsg(ctx, msg)
}

// brokerAt returns the broker's stream timestamp of a worker's registration or
// heartbeat message (kind names which, for the log), which orders those
// messages without regard to any worker's clock: a late redelivery keeps its
// original stamp, a fresh message carries a current one, whatever the worker's
// clock reads. Nil when the message has no usable metadata, in which case it is
// applied unordered.
func (s *Scheduler) brokerAt(ctx context.Context, msg jetstream.Msg, kind, workerID string) *time.Time {
	meta, err := msg.Metadata()
	if err != nil {
		s.logger.DebugContext(
			ctx, "scheduler: "+kind+" message has no broker metadata; applying it unordered",
			slog.String("worker_id", workerID),
			slog.Any("error", err),
		)
		return nil
	}
	if meta.Timestamp.IsZero() {
		return nil
	}
	at := meta.Timestamp.UTC()
	return &at
}

// warnOnExprCapShortfall logs, at most once per distinct shortfall per worker,
// that a registering worker's EXPR limits are below this server's.
//
// It is a diagnostic; the bound itself is the lease gate refusing to lease EXPR
// work to this worker (exprcaps.go). The warning exists because that refusal is
// otherwise visible only on a task, and there may not be one yet.
//
// It is de-duplicated because the worker re-registers on every NATS reconnect
// (internal/worker/registration's Registrar.SetupReconnectHook), and a farm
// that never submits an EXPR template would otherwise log a warning per
// reconnect about work it never runs. The key is the shortfall text itself, so
// a change on either side is reported again; the map is per-process, so a
// server restart also re-reports.
//
// It takes the [store.Worker] the caller has already built, so the comparison
// goes through the same [Scheduler.workerExprShortfall] accessor the two gate
// call sites use.
func (s *Scheduler) warnOnExprCapShortfall(ctx context.Context, w store.Worker) {
	short := s.workerExprShortfall(w)
	if short == "" {
		s.exprCapWarned.Delete(w.ID) // recovered: report again if it recurs
		return
	}
	if prev, ok := s.exprCapWarned.Load(w.ID); ok && prev == short {
		return
	}
	s.exprCapWarned.Store(w.ID, short)
	s.logger.WarnContext(
		ctx, "scheduler: worker registered with EXPR limits tighter than this server's",
		slog.String("worker_id", w.ID),
		slog.String("hostname", w.Hostname),
		slog.String("detail", short),
		slog.String("impact", "this worker is not offered EXPR jobs; it still runs everything else, "+
			"and a farm that submits no EXPR templates is unaffected"),
	)
}

// ensureComputeLocation idempotently registers a compute location reported by a
// worker. Errors are logged and not propagated, because the registry is a
// convenience catalog and must not fail worker registration. An
// empty name is ignored; a name that already exists is left untouched (including
// its admin-curated description); a create that races another registration and
// returns ErrConflict is treated as success.
func (s *Scheduler) ensureComputeLocation(ctx context.Context, name string) {
	if name == "" {
		return
	}
	if _, err := s.store.GetComputeLocationByName(ctx, name); err == nil {
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logger.WarnContext(ctx, "scheduler: lookup compute location failed",
			slog.String("compute_location", name), slog.Any("error", err))
		return
	}

	now := time.Now().UTC()
	_, err := s.store.CreateComputeLocation(ctx, store.ComputeLocation{
		ID:        uuid.NewString(),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if s.stopIfFenced(ctx, err) {
		return // recorded: the registration is not acknowledged
	}
	if err != nil && !errors.Is(err, store.ErrConflict) {
		s.logger.WarnContext(ctx, "scheduler: auto-register compute location failed",
			slog.String("compute_location", name), slog.Any("error", err))
	}
}

// touchWorkerCredential sets LastSeenAt on workerID's active broker
// credential to at and returns the credential, with false when there is none
// or the write failed. Registration is "last seen": connect and reconnect both
// go through it, and it is low-frequency, unlike heartbeat. It is called only
// with broker authentication enabled: with it off there are no credential rows
// and registration does no extra store work.
//
// Like ensureComputeLocation it is best-effort: a credential bookkeeping write
// must not stop a worker coming online. [store.ErrNotFound] (no active
// credential for this worker) is logged at debug, because it is the normal
// shape for a worker that enrolled with broker auth off and was seen after
// auth was turned on, or any other mismatch between workers that exist and
// workers with a credential. Any other error is logged at warn.
func (s *Scheduler) touchWorkerCredential(ctx context.Context, workerID string, at time.Time) (store.WorkerCredential, bool) {
	cred, err := s.store.TouchWorkerCredential(ctx, workerID, at)
	switch {
	case err == nil:
		return cred, true
	case s.stopIfFenced(ctx, err):
		// recorded: the registration is not acknowledged
	case errors.Is(err, store.ErrNotFound):
		s.logger.DebugContext(ctx, "scheduler: no active broker credential to touch on registration",
			slog.String("worker_id", workerID))
	default:
		s.logger.WarnContext(ctx, "scheduler: touch worker credential last-seen failed",
			slog.String("worker_id", workerID), slog.Any("error", err))
	}
	return store.WorkerCredential{}, false
}

// handleWorkerDeregister processes a worker.deregister message published by a
// worker on graceful shutdown. It marks the worker offline immediately (a
// disabled worker stays disabled) so the scheduler stops dispatching
// new assignments to it rather than waiting for the heartbeat-timeout sweep,
// and returns its in-flight tasks to the ready queue, closing their attempts
// and releasing their usage claims. The worker event carries the status the
// store left the row in. A deregister from a process the worker's latest
// registration has replaced (its instance ID differs from the stored one) is
// acked and ignored: applying it would take the new process offline and
// reclaim the tasks it is running. A deregister whose kind is retired or
// preempted then deletes an ephemeral capacity-group member and revokes its
// credential ([Scheduler.removeDepartedEphemeral]). A member the capacity
// actuator was retiring then has its retirement finished
// ([Scheduler.finishRetirementOnDeregister]).
//
// subjectWorkerID is the worker the message's subject attributes it to.
func (s *Scheduler) handleWorkerDeregister(ctx context.Context, msg jetstream.Msg, subjectWorkerID string) {
	// The fields of protocol.DeregisterMsg the server needs; the version is
	// not gated (see the protocol package).
	var m struct {
		WorkerID   string                  `json:"worker_id"`
		InstanceID string                  `json:"instance_id,omitempty"`
		Reason     string                  `json:"reason,omitempty"`
		Kind       protocol.DeregisterKind `json:"kind,omitempty"`
	}
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		// The subject is the only identity left once the body will not decode.
		s.logger.WarnContext(
			ctx, "scheduler: malformed worker.deregister message",
			slog.String("subject_worker_id", subjectWorkerID),
			slog.Any("error", err),
		)
		s.ackMsg(ctx, msg)
		return
	}
	if s.discardOnIdentityMismatch(ctx, msg, subjectWorkerID, m.WorkerID) {
		return
	}

	// Unlike the heartbeat sweep's guarded write, the heartbeat is not
	// re-checked: the worker announced it is leaving. The one guard is the
	// instance ID. The same store call closes its attempts, releases their
	// claims and reclaims its tasks.
	reclaimed, offlined, err := s.store.OfflineWorker(ctx, m.WorkerID, m.InstanceID, time.Now().UTC())
	if s.stopIfFenced(ctx, err) {
		return // unacknowledged, unlike every other error below
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Worker was never registered or already removed — benign race
			// (e.g., deregister arrived before the registration was processed,
			// or the server restarted between registration and shutdown).
			s.logger.WarnContext(
				ctx, "scheduler: deregister for unknown worker — ignoring",
				slog.String("worker_id", m.WorkerID),
			)
		} else {
			s.logger.ErrorContext(
				ctx, "scheduler: mark worker offline on deregister failed",
				slog.String("worker_id", m.WorkerID),
				slog.Any("error", err),
			)
		}
		// Ack in all error cases: neither a not-found nor a store error is
		// likely to resolve on redelivery, and a nack loop would block all
		// subsequent worker messages.
		s.ackMsg(ctx, msg)
		return
	}
	if !offlined {
		// A late or redelivered deregister from a process that has since been
		// replaced. Redelivery cannot make it current, so it is acked.
		s.logger.InfoContext(
			ctx, "scheduler: deregister from a superseded worker process — ignoring",
			slog.String("worker_id", m.WorkerID),
			slog.String("instance_id", m.InstanceID),
		)
		s.ackMsg(ctx, msg)
		return
	}

	attrs := []any{slog.String("worker_id", m.WorkerID), slog.String("reason", m.Reason)}
	if m.Kind != "" {
		attrs = append(attrs, slog.String("kind", string(m.Kind)))
	}
	s.logger.InfoContext(ctx, "scheduler: worker deregistered", attrs...)

	// A gracefully-deregistered worker is offline with nothing in flight, so
	// the heartbeat sweep (which inspects only online workers) will not look at
	// it again. The store call above returned its in-flight tasks to the ready
	// queue; report them.
	s.reclaimOfflineWorkerTasks(ctx, m.WorkerID, "", reclaimOffline, reclaimed)

	// An ephemeral capacity-group member that left for good is deleted now,
	// with its credential (capacitymembership.go). A fenced delete leaves the
	// message unacknowledged, for the new leader.
	removed, fenced := s.removeDepartedEphemeral(ctx, m.WorkerID, m.Kind)
	if fenced {
		return
	}
	if removed {
		s.refreshWorkerGauge(ctx)
		s.ackMsg(ctx, msg)
		return
	}

	status, removed, fenced := s.deregisteredStatus(ctx, m.WorkerID)
	if fenced {
		return
	}
	if removed {
		s.refreshWorkerGauge(ctx)
		s.ackMsg(ctx, msg)
		return
	}
	s.notifier.NotifyWorker(ws.WorkerEvent{
		WorkerID: m.WorkerID,
		Status:   string(status),
	})
	s.refreshWorkerGauge(ctx)
	s.ackMsg(ctx, msg)
}

// ── Heartbeat handler and sweep ──────────────────────────────────────

// handleWorkerHeartbeat processes a worker.heartbeat message by recording, as
// the worker's LastHeartbeatAt, the broker's stream timestamp for it
// ([Scheduler.brokerAt]), or the server's receipt time when the message has
// none. It is never the worker's own At: [Scheduler.sweepStaleWorkers] compares
// LastHeartbeatAt with the server's clock, so a worker whose clock runs
// behind, or is stepped back, would look offline and have its running tasks
// reclaimed and run again. The broker stamp is the time the
// heartbeat was published, so a backlog delivered late (after a store outage
// or a failover) does not make a worker that has since died look alive. The
// same stamp orders heartbeats, so an out-of-order redelivery is ignored. At
// is still what the reconciler checks for a delayed or skewed worker.
//
// The handler reads WorkerID and At and passes AttemptsReported and
// ActiveAttemptIDs to the reconciler (see attemptreconcile.go), but decodes the
// whole [protocol.HeartbeatMsg], including the fields it ignores (active task
// IDs and counts, uptime, last-assignment time). Decoding into a narrower local
// struct would drop every field outside it whatever the version says, which
// defeats the version gate below.
//
// subjectWorkerID is the worker the message's subject attributes it to.
func (s *Scheduler) handleWorkerHeartbeat(ctx context.Context, msg jetstream.Msg, subjectWorkerID string) {
	var m protocol.HeartbeatMsg
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		// The subject is the only identity left once the body will not decode.
		s.logger.WarnContext(
			ctx, "scheduler: malformed worker.heartbeat message",
			slog.String("subject_worker_id", subjectWorkerID),
			slog.Any("error", err),
		)
		s.ackMsg(ctx, msg)
		return
	}
	// Debug level: see discardOnVersionMismatch for why this channel is quiet.
	// The registration warning is the operator-facing signal.
	if s.discardOnVersionMismatch(ctx, msg, slog.LevelDebug, m.Version, m.WorkerID,
		"this worker's liveness signal is not recorded; the heartbeat sweep will retire it") {
		return
	}
	if s.discardOnIdentityMismatch(ctx, msg, subjectWorkerID, m.WorkerID) {
		return
	}

	received := time.Now().UTC()
	at := m.At
	if at.IsZero() {
		at = received
	}

	stamp := s.brokerAt(ctx, msg, "worker.heartbeat", m.WorkerID)
	seen := received
	if stamp != nil {
		seen = *stamp
	}
	if err := s.store.UpdateWorkerHeartbeat(ctx, m.WorkerID, seen, stamp); err != nil {
		if s.stopIfFenced(ctx, err) {
			return
		}
		// ErrNotFound means the worker has not registered yet (e.g. the
		// heartbeat arrived before registration replay completes). Nak so it
		// is retried.
		if errors.Is(err, store.ErrNotFound) {
			s.logger.DebugContext(
				ctx, "scheduler: heartbeat for unknown worker — will retry",
				slog.String("worker_id", m.WorkerID),
			)
			s.nakMsg(ctx, msg)
			return
		}
		s.logger.WarnContext(
			ctx, "scheduler: update heartbeat failed",
			slog.String("worker_id", m.WorkerID),
			slog.Any("error", err),
		)
		s.nakMsg(ctx, msg)
		return
	}
	// Keyed on AttemptsReported, not on the list: a worker holding nothing
	// sends an empty list, and that empty report is how the reconciler learns
	// an attempt is no longer held. A worker that sends neither field is never
	// reconciled.
	//
	// A list longer than any worker can hold is ignored, leaving the worker's
	// last report to go stale, so its tasks are not reconciled until it sends
	// a list of a sane length.
	switch {
	case s.reconciler == nil || !m.AttemptsReported:
	case len(m.ActiveAttemptIDs) > maxReportedAttempts:
		s.logger.WarnContext(ctx, "scheduler: worker heartbeat lists more attempts than a worker can hold; "+
			"ignoring its attempt list, and its tasks are not reconciled until it sends a shorter one",
			slog.String("worker_id", m.WorkerID), slog.Int("attempts", len(m.ActiveAttemptIDs)),
			slog.Int("limit", maxReportedAttempts))
	case s.reconciler.record(m.WorkerID, at, received, m.ActiveAttemptIDs):
		s.logger.WarnContext(ctx, "scheduler: worker heartbeat delayed or skewed: more than heartbeat_timeout "+
			"between the worker's timestamp and its receipt; its tasks are not reconciled until its "+
			"heartbeats arrive in time again",
			slog.String("worker_id", m.WorkerID), slog.Time("worker_at", at))
	}
	s.ackMsg(ctx, msg)
}

// runHeartbeatSweep periodically finds workers whose last heartbeat is older
// than WorkerTimeout, marks them offline, and reclaims their in-flight tasks.
// On the same tick it reaps tasks stranded in 'assigned' on still-live workers,
// reclaims attempts a live worker has stopped reporting ([reconcileAttempts]),
// demotes stalled jobs, flags/clears ready tasks no online worker can satisfy
// ([sweepUnschedulable]), evaluates the enabled capacity groups
// ([Scheduler.sweepCapacity]), re-evaluates cross-job dependencies for any job
// stuck blocked ([sweepBlockedJobs], the backstop for
// [Scheduler.ReconcileDependents]), and refreshes the queue-depth, idle-worker
// and usage-claim Prometheus gauges.
func (s *Scheduler) runHeartbeatSweep(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.HeartbeatSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce is one tick of [runHeartbeatSweep]. A sweep whose write is
// fenced records it and returns, and the tick stops there; a fenced scheduler
// sweeps nothing more.
func (s *Scheduler) sweepOnce(ctx context.Context) {
	for _, sweep := range []func(context.Context){
		s.sweepStaleWorkers,
		s.sweepRetiredWorkers,
		s.sweepRetiredJobs,
		s.reapStaleAssignedTasks,
		// Before the demotion, so a job left with nothing in flight by a
		// reconciliation reclaim is demoted on the same tick.
		func(ctx context.Context) { s.reconcileAttempts(ctx, time.Now().UTC()) },
		s.demoteStalledJobs,
		s.sweepUnschedulable,
		s.sweepCapacity,
		s.sweepBlockedJobsOnce,
		// The gauges refresh on the same tick, so there is no separate
		// metrics loop.
		s.refreshQueueDepthGauge,
		s.refreshIdleWorkerGauge,
		s.refreshUsageClaimGauge,
	} {
		if s.deposed() {
			return
		}
		sweep(ctx)
	}
}

// sweepBlockedJobsOnce is the blocked-job sweep's place in a tick.
func (s *Scheduler) sweepBlockedJobsOnce(ctx context.Context) {
	if err := s.sweepBlockedJobs(ctx); err != nil && !s.stopIfFenced(ctx, err) {
		s.logger.ErrorContext(ctx, "scheduler: sweep blocked jobs failed", slog.Any("error", err))
	}
}

// demoteStalledJobs reconciles the JobStatusRunning invariant after a sweep or
// reap. A job whose last assigned/running task was just returned to the ready
// queue (worker died, or assignment reaped) is left marked 'running' even
// though nothing is in flight; this returns such jobs to 'pending' until a
// worker picks the work back up. It is idempotent: a job with work still in
// flight, or already terminal, is untouched. A sweep that fails part-way still
// returns the jobs it had already demoted (each demotion is its own committed
// transaction), so those are announced before the error is handled: they are
// pending now and no later sweep will list them again.
func (s *Scheduler) demoteStalledJobs(ctx context.Context) {
	demoted, err := s.store.DemoteStalledJobs(ctx, time.Now().UTC())
	s.announceDemoted(ctx, demoted)
	s.warnUnlessStopped(ctx, err, "scheduler: demote stalled jobs failed")
}

// announceDemoted logs and fans out a pending JobEvent for each demoted job.
func (s *Scheduler) announceDemoted(ctx context.Context, demoted []string) {
	now := time.Now().UTC()
	for _, jobID := range demoted {
		s.logger.InfoContext(
			ctx, "scheduler: demoted stalled job to pending (no in-flight tasks)",
			slog.String("job_id", jobID),
		)
		s.notifier.NotifyJob(ws.JobEvent{
			JobID:     jobID,
			Status:    string(store.JobStatusPending),
			UpdatedAt: now,
		})
	}
}

// reapStaleAssignedTasks returns tasks that have been stuck in 'assigned' longer
// than AssignedTaskTimeout to the ready queue. Unlike sweepStaleWorkers it does
// not key on worker liveness: a task can be lost in 'assigned' on a worker that
// is still heartbeating (e.g. the worker crashed between lease and process
// start), and nothing else recovers it. The store closes each reclaimed task's
// attempt and releases its usage-pool claims inside the same transaction that
// returns the task to ready (invariant I3), and reports exactly the tasks it
// reclaimed, so a task that is leased again right afterwards keeps its new
// attempt and claims. A reap that fails part-way still returns the tasks it had
// already reclaimed (the store commits them a batch at a time), so those are
// announced before the error is handled: they are ready now and no later reap
// will list them again.
//
// While the bus is down, or has been up for less than BusReconnectGrace, it
// reclaims nothing (see [Scheduler.livenessDeferred]): the worker's running
// report may be waiting in the stream, and reclaiming now re-dispatches a task
// that is already running.
func (s *Scheduler) reapStaleAssignedTasks(ctx context.Context) {
	if deferred, _ := s.livenessDeferred(time.Now()); deferred {
		return
	}
	cutoff := time.Now().UTC().Add(-s.cfg.AssignedTaskTimeout)
	reclaimed, err := s.store.ReclaimStaleAssignedTasks(ctx, cutoff)
	s.announceReaped(ctx, reclaimed)
	s.warnUnlessStopped(ctx, err, "scheduler: reclaim stale assigned tasks failed")
}

// announceReaped logs the reap and fans out a ready TaskEvent for each
// reclaimed task, waking the lease waiters on its queue.
func (s *Scheduler) announceReaped(ctx context.Context, reclaimed []store.Task) {
	if len(reclaimed) == 0 {
		return
	}

	s.logger.WarnContext(
		ctx, "scheduler: reaped tasks stuck in assigned — returning to ready queue",
		slog.Int("count", len(reclaimed)),
		slog.Duration("assigned_task_timeout", s.cfg.AssignedTaskTimeout),
	)

	now := time.Now().UTC()
	for _, task := range reclaimed {
		s.notifyTaskReclaimed(ctx, task, now)
	}
}

// notifyTaskReclaimed announces a task a reclaim returned to ready and wakes
// the lease waiters on its job's queue.
func (s *Scheduler) notifyTaskReclaimed(ctx context.Context, task store.Task, now time.Time) {
	s.notifier.NotifyTask(ws.TaskEvent{
		JobID:     task.JobID,
		TaskID:    task.ID,
		Name:      task.Name,
		Status:    string(store.TaskStatusReady),
		UpdatedAt: now,
	})
	s.notifyQueueForJob(ctx, task.JobID)
}

// sweepStaleWorkers finds workers whose heartbeat has expired, marks them
// offline, reclaims their assigned/running tasks, and refreshes the
// WorkersTotal gauge. A disabled worker is swept like any other and stays
// disabled, so its event carries its effective status, disabled,
// not offline: the event still tells clients it changed (it is now
// removable).
//
// The list of stale workers is a candidate list only. By the time each candidate is
// handled its heartbeat may have arrived, or it may have re-registered, so the
// offline transition is [store.WorkerStore.OfflineStaleWorker], which re-checks
// the heartbeat inside its own write and leaves such a worker online with its
// tasks running. Only a worker the store actually took offline is announced
// and has its reclaim reported; the attempts, claims and tasks are handled by
// that same store call.
//
// The sweep decides nothing while the bus is down or has been up for less than
// BusReconnectGrace ([Scheduler.livenessDeferred]): a heartbeat travels over the
// bus, so a broker outage looks like every worker going silent at once, and
// acting on it would reclaim tasks that are still running. It logs once per
// outage and once per grace window ([Scheduler.logLivenessDeferred]);
// [Scheduler.reapStaleAssignedTasks] defers silently on the same tick.
func (s *Scheduler) sweepStaleWorkers(ctx context.Context) {
	if deferred, up := s.livenessDeferred(time.Now()); deferred {
		s.logLivenessDeferred(ctx, up)
		return
	}
	cutoff := time.Now().UTC().Add(-s.cfg.WorkerTimeout)
	stale, err := s.store.ListStaleWorkers(ctx, cutoff)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: list stale workers failed", slog.Any("error", err))
		}
		return
	}
	if len(stale) == 0 {
		return
	}

	s.logger.InfoContext(
		ctx, "scheduler: heartbeat sweep found stale workers",
		slog.Int("count", len(stale)),
	)

	for _, w := range stale {
		// Mark the worker offline and hand its in-flight work back in one store
		// call. The guard re-checks the heartbeat against the same cutoff the list
		// used, so a heartbeat or a re-registration that landed after the list
		// keeps the worker online (invariant I1).
		reclaimed, marked, err := s.store.OfflineStaleWorker(ctx, w.ID, cutoff, time.Now().UTC())
		if s.stopIfFenced(ctx, err) {
			return
		}
		if err != nil {
			s.logger.WarnContext(
				ctx, "scheduler: mark worker offline failed",
				slog.String("worker_id", w.ID),
				slog.Any("error", err),
			)
			continue
		}
		if !marked {
			s.logger.DebugContext(
				ctx, "scheduler: worker no longer a stale candidate, left as is",
				slog.String("worker_id", w.ID),
			)
			continue
		}
		// The flag is the one listed: an enable or disable racing the sweep is
		// reported by its own endpoint's response, and the store is correct.
		offline := w
		offline.Status = store.WorkerStatusOffline
		s.notifier.NotifyWorker(ws.WorkerEvent{
			WorkerID: w.ID,
			Name:     w.Name,
			Hostname: w.Hostname,
			FarmID:   w.FarmID,
			Status:   string(offline.EffectiveStatus()),
		})

		s.reclaimOfflineWorkerTasks(ctx, w.ID, w.Hostname, reclaimOffline, reclaimed)
	}

	s.refreshWorkerGauge(ctx)
}

// livenessDeferred reports whether, at now, the sweeps that judge a worker by
// its heartbeat must hold off: the bus is down, or came back less than
// BusReconnectGrace ago, so a live worker has not yet had time to notice the
// outage ended, redial and heartbeat. A dead worker is declared offline only
// once the grace has passed: exactly BusReconnectGrace after the reconnect the
// sweep acts. With no BusUpSince source it is never deferred. The grace counts
// from the bus coming up or from Run starting, whichever is later.
//
// It also returns the time it judged by, zero while the bus is down, and
// remembers the first non-zero one, the connection made at startup.
func (s *Scheduler) livenessDeferred(now time.Time) (deferred bool, up time.Time) {
	if s.cfg.BusUpSince == nil {
		return false, time.Time{}
	}
	up = s.cfg.BusUpSince()
	if up.IsZero() {
		return true, up
	}
	// A server that became the leader long after its bus connection came up
	// (a promoted standby) has not consumed any heartbeat yet: they are all
	// still waiting in the stream. Its grace counts from when Run started.
	// Without HA, Run starts just after the bus comes up, so this moves the
	// grace by milliseconds at most.
	up = latest(up, s.startedAt)
	s.deferLog.mu.Lock()
	if s.deferLog.firstUp.IsZero() {
		s.deferLog.firstUp = up
	}
	s.deferLog.mu.Unlock()
	return now.Sub(up) < s.cfg.BusReconnectGrace, up
}

// latest returns the later of a and b.
func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// logLivenessDeferred logs a deferred liveness sweep once per outage and once
// per grace window, not on every tick: the sweep runs every 15 s, and the
// grace lasts about 189 s at the defaults after every start. The window
// after the connection made at startup is reported as startup, not as a
// reconnect. up is the bus-up time [Scheduler.livenessDeferred] judged by,
// zero while the bus is down.
func (s *Scheduler) logLivenessDeferred(ctx context.Context, up time.Time) {
	busDown := up.IsZero()
	d := &s.deferLog
	d.mu.Lock()
	if d.logged && d.key.Equal(up) {
		d.mu.Unlock()
		return
	}
	d.logged, d.key = true, up
	startup := !busDown && up.Equal(d.firstUp)
	d.mu.Unlock()

	grace := slog.Duration("bus_reconnect_grace", s.cfg.BusReconnectGrace)
	switch {
	case busDown:
		s.logger.InfoContext(ctx, "scheduler: bus is down; liveness sweep deferred until it reconnects",
			slog.Bool("bus_down", true), grace)
	case startup:
		s.logger.InfoContext(ctx, "scheduler: bus connected at startup; liveness sweep deferred for the reconnect grace",
			slog.Bool("bus_down", false), grace, slog.Time("resumes_at", up.Add(s.cfg.BusReconnectGrace)))
	default:
		s.logger.InfoContext(ctx, "scheduler: bus reconnected recently; liveness sweep deferred",
			slog.Bool("bus_down", false), grace, slog.Time("resumes_at", up.Add(s.cfg.BusReconnectGrace)))
	}
}

// sweepRetiredWorkers hard-deletes workers that have been offline longer than
// OfflineWorkerRetention, bounding the worker table's growth on farms with
// ephemeral nodes. An offline worker's in-flight tasks were reclaimed when it
// went offline, so no reclaim is needed here. Disabled and online
// workers are never auto-removed. A non-positive retention disables the sweep.
func (s *Scheduler) sweepRetiredWorkers(ctx context.Context) {
	if s.cfg.OfflineWorkerRetention <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.cfg.OfflineWorkerRetention)
	removed, err := s.store.DeleteOfflineWorkersBefore(ctx, cutoff)
	if err != nil {
		s.warnUnlessStopped(ctx, err, "scheduler: retention sweep failed")
		return
	}
	if len(removed) == 0 {
		return
	}

	s.logger.InfoContext(
		ctx, "scheduler: retention sweep removed offline workers",
		slog.Int("count", len(removed)),
		slog.Duration("retention", s.cfg.OfflineWorkerRetention),
	)

	for _, w := range removed {
		s.announceRemovedWorker(w)
	}

	s.refreshWorkerGauge(ctx)
}

// sweepRetiredJobs hard-deletes terminal jobs older than JobRetention along with
// all of their data, bounding table growth. completed and canceled jobs are
// always eligible; failed jobs only when JobRetentionIncludeFailed is set.
// Active jobs are never touched. A non-positive retention disables the sweep.
func (s *Scheduler) sweepRetiredJobs(ctx context.Context) {
	if s.cfg.JobRetention <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.cfg.JobRetention)
	// On error, removed still lists the jobs deleted before it: they are gone
	// and are announced like any others.
	removed, err := s.store.DeleteTerminalJobsBefore(ctx, cutoff, s.cfg.JobRetentionIncludeFailed)
	s.warnUnlessStopped(ctx, err, "scheduler: job retention sweep failed")
	if len(removed) == 0 {
		return
	}

	s.logger.InfoContext(
		ctx, "scheduler: retention sweep removed terminal jobs",
		slog.Int("count", len(removed)),
		slog.Duration("retention", s.cfg.JobRetention),
	)

	now := time.Now().UTC()
	for _, j := range removed {
		s.notifier.NotifyJob(ws.JobEvent{
			JobID:     j.ID,
			Name:      j.Name,
			QueueID:   j.QueueID,
			Status:    ws.JobStatusRemoved,
			UpdatedAt: now,
		})
	}
}

// reclaimCause says why the store reclaimed a worker's tasks. It selects only
// the wording [Scheduler.reclaimOfflineWorkerTasks] logs; the reporting is the
// same for every cause.
type reclaimCause int

const (
	// reclaimOffline: the worker was taken offline (heartbeat sweep or
	// graceful deregister).
	reclaimOffline reclaimCause = iota
	// reclaimRestart: the worker re-registered from a new process (a changed
	// instance ID) and stays online; only its previous process's
	// tasks were reclaimed.
	reclaimRestart
)

// reclaimOfflineWorkerTasks reports the tasks the store returned to the ready
// queue when it took workerID offline (the heartbeat sweep and the
// graceful-deregister handler, cause reclaimOffline) or when the worker
// re-registered from a new process (the register handler, cause
// reclaimRestart). By the time it runs the store has closed the worker's
// running attempts, released their usage claims and returned its
// assigned/running tasks to ready, all in the transaction that marked the
// worker offline or recorded its new instance (invariant I3). The tasks
// therefore are not left in 'assigned'/'running' (the heartbeat sweep considers
// only workers still marked online, and a restarted worker's new process never
// reports on them), and no usage-pool claim stays held on behalf of a worker
// process that is gone.
func (s *Scheduler) reclaimOfflineWorkerTasks(ctx context.Context, workerID, hostname string, cause reclaimCause, reclaimed []store.Task) {
	if len(reclaimed) == 0 {
		if cause == reclaimOffline {
			s.logger.InfoContext(
				ctx, "scheduler: worker marked offline (no tasks to reclaim)",
				slog.String("worker_id", workerID),
				slog.String("hostname", hostname),
			)
		}
		return
	}
	msg := "scheduler: reclaimed tasks from offline worker"
	if cause == reclaimRestart {
		msg = "scheduler: reclaimed tasks from a restarted worker"
	}
	s.logger.InfoContext(
		ctx, msg,
		slog.String("worker_id", workerID),
		slog.String("hostname", hostname),
		slog.Int("tasks_reclaimed", len(reclaimed)),
	)
	// Reclaimed tasks are back to ready but there are no job IDs to scope a
	// per-queue wake, so wake every waiter and parked workers re-lease promptly.
	s.waiters.notifyAll()
}

// WakeQueue wakes any parked lease waiters on queueID. Called by the API job
// handler after a successful submission so newly-ready tasks are leased without
// waiting out the long-poll hold. It also wakes queue-unaffiliated workers
// parked under [bus.WildcardQueueToken], since they are eligible for any queue's
// work.
func (s *Scheduler) WakeQueue(queueID string) {
	s.waiters.notify(queueID)
	if queueID != bus.WildcardQueueToken {
		s.waiters.notify(bus.WildcardQueueToken)
	}
}

// WakeWorker wakes the lease requests a worker that may not take new work
// (disabled, retiring or asleep) has parked, so a worker that has just become
// able to is leased work at once rather than after the rest of its
// leaseHoldTimeout. Called by the API's enable handler after the write, and
// when a woken capacity-group member registers ([Scheduler.noteMemberWoke]). A
// woken request re-reads the worker, so a wake for a worker that still may not
// take work leases nothing.
func (s *Scheduler) WakeWorker(workerID string) {
	s.disabledWaiters.notify(workerID)
}

// notifyQueueForJob wakes any parked lease waiters on the job's queue (and any
// queue-unaffiliated workers). A missed wake is corrected when the worker's
// outstanding lease request is re-issued after leaseHoldTimeout.
func (s *Scheduler) notifyQueueForJob(ctx context.Context, jobID string) {
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return
	}
	s.WakeQueue(job.QueueID)
}

// refreshWorkerGauge reads current worker counts from the store and sets the
// WorkersTotal Prometheus gauge accordingly. Called after any event that can
// change worker status (registration, sweep).
func (s *Scheduler) refreshWorkerGauge(ctx context.Context) {
	for _, status := range []store.WorkerStatus{
		store.WorkerStatusOnline,
		store.WorkerStatusOffline,
		store.WorkerStatusDisabled,
	} {
		opts := store.ListWorkersOptions{
			Status:     status,
			Pagination: store.Pagination{Limit: 1, Offset: 0},
		}
		opts.Pagination.Validate() //nolint:errcheck // Validate only clamps values; it never returns a non-nil error

		// Total from a one-row page gives the count without fetching all rows.
		page, err := s.store.ListWorkers(ctx, opts)
		if err != nil {
			s.logger.WarnContext(
				ctx, "scheduler: refresh worker gauge failed",
				slog.String("status", string(status)),
				slog.Any("error", err),
			)
			continue
		}
		s.metrics.WorkersTotal.WithLabelValues(string(status)).Set(float64(page.Total))
	}
}

// ── Instrumentation helpers ─────────────────────────────────────────

// refreshQueueDepthGauge queries the store for the current number of leasable
// ready tasks per queue in the scheduler's farm (backoff elapsed, queue
// unpaused, job schedulable), updates the SchedulerQueueDepth Prometheus
// gauge, and wakes lease waiters on every queue with leasable work. The wake
// backs up scheduleRetryWake's in-process backoff timers across a restart.
//
// A queue the store no longer reports (its last ready task was leased) is set
// to 0 rather than dropped, so a scraper sees the depth fall.
//
// Called on every heartbeat-sweep tick. Errors are logged at WARN level and do
// not abort the sweep.
func (s *Scheduler) refreshQueueDepthGauge(ctx context.Context) {
	counts, err := s.store.CountReadyTasksByQueue(ctx, s.cfg.FarmID, time.Now().UTC())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: refresh queue depth gauge failed", slog.Any("error", err))
		}
		return
	}
	if s.queueDepthSeen == nil {
		s.queueDepthSeen = make(map[string]struct{})
	}
	for queueID, n := range counts {
		s.queueDepthSeen[queueID] = struct{}{}
		s.metrics.SchedulerQueueDepth.WithLabelValues(queueID).Set(float64(n))
		if n > 0 {
			s.WakeQueue(queueID)
		}
	}
	for queueID := range s.queueDepthSeen {
		if _, ok := counts[queueID]; !ok {
			s.metrics.SchedulerQueueDepth.WithLabelValues(queueID).Set(0)
		}
	}
}

// refreshUsageClaimGauge queries the store for all configured usage pools and
// the active claim count for each, then updates the UsageActiveClaims Prometheus
// gauge.
//
// Called on every heartbeat-sweep tick. Errors are logged and do not abort the
// sweep; the gauge keeps its last value.
func (s *Scheduler) refreshUsageClaimGauge(ctx context.Context) {
	pools, err := s.store.ListUsagePools(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: refresh usage claim gauge: list pools failed", slog.Any("error", err))
		}
		return
	}

	for _, pool := range pools {
		n, err := s.store.ActiveClaimCount(ctx, pool.ID)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(
					ctx, "scheduler: refresh usage claim gauge: count failed",
					slog.String("pool_id", pool.ID),
					slog.String("pool_name", pool.Name),
					slog.Any("error", err),
				)
			}
			continue
		}
		s.metrics.UsageActiveClaims.WithLabelValues(pool.Name).Set(float64(n))
	}
}

// refreshIdleWorkerGauge queries the store for the count of online workers
// in the scheduler's farm that have no active task, and updates the
// SchedulerIdleWorkers Prometheus gauge.
//
// Called on every heartbeat-sweep tick and after any worker-status change event.
// Errors are logged at WARN level and do not abort the caller.
func (s *Scheduler) refreshIdleWorkerGauge(ctx context.Context) {
	n, err := s.store.CountIdleWorkers(ctx, s.cfg.FarmID)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: refresh idle worker gauge failed", slog.Any("error", err))
		}
		return
	}
	s.metrics.SchedulerIdleWorkers.WithLabelValues(s.cfg.FarmID).Set(float64(n))
}

// ── NATS ack/nak helpers ──────────────────────────────────────────────────────

// ackMsg acknowledges msg and logs a warning if the ack itself fails, since a
// failed ack can indicate a connection problem.
//
// A scheduler whose store writes have been fenced acknowledges nothing: the
// message stays unacknowledged, and JetStream redelivers it to the new leader
// once its ack wait runs out.
func (s *Scheduler) ackMsg(ctx context.Context, msg jetstream.Msg) {
	if s.leftForNewLeader(ctx, msg) {
		return
	}
	if err := msg.Ack(); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: ack message failed",
			slog.String("subject", msg.Subject()),
			slog.Any("error", err),
		)
	}
}

// nakMsg negatively-acknowledges msg, asking for it to be redelivered after
// [nakDelay], and logs a warning if the nak itself fails. Like
// [Scheduler.ackMsg] it leaves msg alone once fenced: a nak would spend one
// of the message's deliveries on this server, which can no longer process
// it, and hold it back from the new leader for the delay; left
// unacknowledged, it goes to the new leader when its ack wait runs out.
func (s *Scheduler) nakMsg(ctx context.Context, msg jetstream.Msg) {
	if s.leftForNewLeader(ctx, msg) {
		return
	}
	var delivered uint64
	if md, err := msg.Metadata(); err == nil {
		delivered = md.NumDelivered
	}
	if err := msg.NakWithDelay(nakDelay(delivered)); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: nak message failed",
			slog.String("subject", msg.Subject()),
			slog.Any("error", err),
		)
	}
}

// nakBaseDelay and nakMaxDelay bound how long a nak'd message waits to be
// redelivered.
const (
	nakBaseDelay = time.Second
	nakMaxDelay  = 30 * time.Second
)

// nakDelay is how long a message that has been delivered n times (its
// NumDelivered) waits before its next delivery: nakBaseDelay, doubling with
// each delivery, capped at nakMaxDelay. A failure is usually the store being
// unreachable, and a nak without a delay is redelivered at once, which spends a
// consumer's whole MaxDeliver budget in milliseconds and drops the message (a
// task's terminal status, say). With the backoff the server's consumers ride
// out minutes of outage, which covers a PostgreSQL restart and an HA failover
// while the database is down.
func nakDelay(n uint64) time.Duration {
	d := nakBaseDelay
	for i := uint64(1); i < n && d < nakMaxDelay; i++ {
		d *= 2
	}
	return min(d, nakMaxDelay)
}

// leftForNewLeader reports whether this scheduler has been fenced, in which
// case msg is left unacknowledged for the new leader.
func (s *Scheduler) leftForNewLeader(ctx context.Context, msg jetstream.Msg) bool {
	if !s.deposed() {
		return false
	}
	s.logger.DebugContext(ctx, "scheduler: not acknowledging a message; this server is no longer the leader",
		slog.String("subject", msg.Subject()))
	return true
}
