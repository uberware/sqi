// SPDX-License-Identifier: AGPL-3.0-or-later

// Package scheduler implements the sqi-server assignment loop and worker
// registry — the authoritative component for deciding which task runs on which
// worker.
//
// The [Scheduler] runs three concurrent loops:
//
//  1. Lease subscriber: a core-NATS request/reply subscriber on work.lease.>
//     ([handleLeaseRequest]). Idle workers ask for work; the scheduler selects a
//     priority-ordered batch of ready tasks the worker is eligible for that fits
//     its free CPU cores ([selectLeaseBatch]), atomically leases each
//     ([store.TaskStore.LeaseReadyTask]), and replies with the assignment
//     payloads. When no work is available the request parks in the waiter
//     registry until new work appears or the hold elapses, then replies.
//
//  2. Worker registry: a NATS push-consumer for worker.register messages that
//     persists capability data via [store.WorkerStore.RegisterWorker] and keeps
//     the WorkersTotal Prometheus gauge current.
//
//  3. Heartbeat sweep: a NATS push-consumer that updates each worker's
//     LastHeartbeatAt on worker.heartbeat messages, paired with a periodic
//     timer that marks workers offline once their heartbeat goes stale
//     ([store.WorkerStore.ListStaleWorkers]), terminates their open attempts
//     ([store.TaskAttemptStore.TerminateWorkerAttempts]), and returns their
//     in-flight tasks to the ready queue ([store.TaskStore.ReclaimWorkerTasks]).
//     The same tick refreshes the queue-depth, idle-worker, and usage-claim
//     Prometheus gauges.
//
// Worker selection. A task is matched to a worker by capability tags,
// compute-location affinity, and queue/farm filtering ([WorkerEligible]),
// subject to per-queue and per-farm maximum-concurrent-task limits
// ([policyGate]). Once a worker is chosen, a provisional [store.TaskAttempt] is
// created and any required usage pool slots are claimed atomically
// ([store.UsageClaimStore.TryClaimSlots]); if the pool is saturated
// the assignment is rolled back and the task stays ready for the next tick.
// Attempt numbers come from [store.TaskAttemptStore.LatestTaskAttempt] — 1 for
// a fresh task, N+1 on retry.
//
// Assignment payload. [buildAssignPayload] re-parses the job's raw OpenJD
// template to extract the matching step's OnRun action, embedded files, and
// ordered environments (job environments first, then step, per the OpenJD
// spec). The path-map field is reserved but empty until named storage location
// CRUD and resolved-mode path translation are implemented.
//
// Status and log ingestion. A push-consumer on the SQI_TASK stream
// ([handleTaskStatusMessage]) decodes [protocol.TaskStatusMsg] from
// task.status.<job>, updates the task/attempt, releases held usage pool slots, and
// drives step/job completion including [openjd.ResolveDependencies] for
// multi-step jobs. A push-consumer on SQI_LOGS ([handleLogChunk]) persists each
// task.logs.<task> chunk as a [store.TaskLog] row, recording both the
// worker-assigned sequence number and the NATS stream sequence that serves as
// the log-tail pagination cursor.
//
// Cancellation. [CancelJob] and [CancelTask] are the server-side entry points
// called by the REST layer: they close running attempts, transition tasks to
// [store.TaskStatusCanceled], publish task.cancel.<taskID> signals to assigned
// workers ([bus.Client.PublishTaskCancel]), and release held usage pool slots. The
// logic lives in cancellation.go; the SQI_CANCEL stream and publish helper live
// in the bus package.
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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// DefaultConfig returns a [Config] with conservative production-safe defaults.
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
	// API/UI). A value <= 0 disables the sweep entirely — unlike the other
	// duration knobs above, this is NOT coerced up to the default in [New],
	// since 0 is a legitimate "off" setting. Default: 30 s.
	UnschedulableGrace time.Duration

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
	// like UnschedulableGrace, it is NOT coerced up in [New]. Default: 0.
	DefaultFailureLimit int

	// ExprLimits are the OpenJD EXPR limits this server enforces when it
	// ACCEPTS a template (internal/config's openjd.expr_* keys). The scheduler
	// does not evaluate expressions; it needs them only to compare against each
	// worker's advertised caps before dispatching an EXPR job — see
	// exprcaps.go. It must be the same value the submitter is built with, which
	// is why internal/server sets both from one field rather than letting a
	// caller populate them independently.
	//
	// Zero fields normalize to the defaults in [New].
	ExprLimits openjd.ExprLimits
}

// busClient is the subset of [bus.Client] used by the Scheduler. Defined as
// an interface so unit tests can inject a stub without a real NATS broker.
type busClient interface {
	ConsumeWorker(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error)
	ConsumeTaskStatus(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error)
	ConsumeTaskLogs(ctx context.Context, handler jetstream.MessageHandler) (jetstream.ConsumeContext, error)
	PublishTaskCancel(ctx context.Context, taskID string, data []byte) error
	SubscribeWorkerDiag(handler func(subject string, data []byte)) (*nats.Subscription, error)
	SubscribeLease(handler func(queueID string, data []byte) []byte) (*nats.Subscription, error)
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

	// leaseLocks serializes lease selection per worker. Concurrent lease
	// requests for the SAME worker (one outstanding request per queue it
	// serves, plus retry overlap) must not both read the same committed-core
	// count and each lease up to free, over-committing the worker. The lock is
	// held only around selectLeaseBatch, never across the long-poll park.
	leaseLocks sync.Map // workerID -> *sync.Mutex

	// exprCapWarned de-duplicates the registration-time EXPR-limit warning:
	// workerID -> the last shortfall text logged for it. See
	// [Scheduler.warnOnExprCapShortfall]. Entries are cleared when a worker
	// stops being short but NOT when it is deleted, so the map retains one
	// short string per distinct worker ID that has ever registered short with
	// this process -- bounded, not pruned, and gone on restart.
	exprCapWarned sync.Map // workerID -> string

	// leaseHoldTimeout bounds how long an unfulfillable lease request parks
	// before replying empty. Overridable in tests.
	leaseHoldTimeout time.Duration

	// wg tracks all internal goroutines so [Run] can wait for clean exit.
	wg sync.WaitGroup

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
}

// New creates a Scheduler. Call [Run] to start its goroutines.
//
// notifier receives live-event notifications after each state change so the
// WebSocket hub can fan them out to subscribed clients. Pass
// [ws.NoopNotifier] (or nil — treated as NoopNotifier) when no WebSocket hub
// is wired.
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
	// Normalized ONCE here so exprCapShortfall compares against the values a
	// submission would actually be metered under. An unset field left at 0
	// would report "no shortfall" in that dimension for every worker.
	cfg.ExprLimits = cfg.ExprLimits.Normalized()
	n := notifier
	if n == nil {
		n = ws.NoopNotifier{}
	}
	return &Scheduler{
		cfg:              cfg,
		store:            st,
		bus:              busClient,
		metrics:          m,
		logger:           logger,
		notifier:         n,
		diagBuf:          diagBuf,
		waiters:          newWaiterRegistry(),
		leaseHoldTimeout: 30 * time.Second,
		retryWakeTimers:  make(map[*time.Timer]struct{}),
		// ctx is overwritten with the derived cancellable context in Run.
		// The background fallback ensures NATS callbacks can't nil-panic if
		// somehow invoked before Run (e.g. in a partial test setup).
		ctx: context.Background(),
	}
}

// WorkerTimeout returns the effective heartbeat-timeout threshold after
// normalization. The API layer uses it to decide whether a disabled worker is
// dead (and therefore removable) from its last-heartbeat age.
func (s *Scheduler) WorkerTimeout() time.Duration {
	return s.cfg.WorkerTimeout
}

// Run starts all scheduler goroutines and blocks until ctx is canceled.
// It returns after all goroutines have exited cleanly.
func (s *Scheduler) Run(ctx context.Context) error {
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
	)

	// ── Worker NATS consumer ────────────────────────────────
	// A single JetStream push-consumer delivers both worker.register and
	// worker.heartbeat messages. The handler dispatches by subject.
	_, err := s.bus.ConsumeWorker(ctx, s.handleWorkerMessage)
	if err != nil {
		return fmt.Errorf("scheduler: start worker consumer: %w", err)
	}
	s.logger.InfoContext(ctx, "scheduler: worker consumer started")

	// ── Task-status NATS consumer ────────────────────────────────
	// A JetStream push-consumer on SQI_TASK delivers task.status.<job>
	// messages from workers. handleTaskStatusMessage updates the store,
	// closes attempt records, releases usage pool slots, and drives step/job
	// completion.
	if err := s.startTaskStatusConsumer(ctx); err != nil {
		return fmt.Errorf("scheduler: start task-status consumer: %w", err)
	}
	s.logger.InfoContext(ctx, "scheduler: task-status consumer started")

	// ── Task-logs NATS consumer ──────────────────────────────────
	// A JetStream push-consumer on SQI_LOGS delivers task.logs.<task>
	// messages from workers. handleLogChunk persists each chunk to the
	// task_logs table with NATS sequence as the pagination cursor.
	if err := s.startTaskLogsConsumer(ctx); err != nil {
		return fmt.Errorf("scheduler: start task-logs consumer: %w", err)
	}
	s.logger.InfoContext(ctx, "scheduler: task-logs consumer started")

	// ── Worker diagnostic-log subscriber ─────────────────────────
	// A core-NATS subscriber on worker.diag.> decodes worker diagnostic
	// records into the in-memory ring buffer. No-op when diagnostics are
	// disabled (diagBuf nil).
	if err := s.startDiagConsumer(); err != nil {
		return fmt.Errorf("scheduler: start diagnostic-log consumer: %w", err)
	}
	if s.diagBuf != nil {
		s.logger.InfoContext(ctx, "scheduler: diagnostic-log consumer started")
	}

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
	if s.diagSub != nil {
		if err := s.diagSub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
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

// errNoWorkerAvailable signals that a task could not be leased because no
// eligible worker/capacity was available (or a usage pool was saturated). It is
// a skip signal, not a logged warning — the task simply stays ready for the next
// lease request. Used by the lease path (lease.go) and the usage-claim helper.
var errNoWorkerAvailable = errors.New("no worker available")

// createAttemptAndClaimUsage creates a provisional [store.TaskAttempt] for
// the assignment and atomically claims any required usage pool slots.
//
// If either operation fails the task's status is reverted to
// [store.TaskStatusReady] so it is re-queued on the next lease request.
// [errNoWorkerAvailable] is returned when a usage pool is at capacity so
// the caller skips logging a warning.
func (s *Scheduler) createAttemptAndClaimUsage(
	ctx context.Context,
	task store.Task,
	worker store.Worker,
	step store.Step,
	pools map[string]store.UsagePool,
	now time.Time,
) (store.TaskAttempt, error) {
	// The attempt record must exist before usage claims can be created
	// (FK constraint). Determine the next AttemptNumber from the latest existing
	// attempt so that retries are numbered correctly (1 for a fresh task, N+1
	// on each subsequent retry).
	nextNum, err := s.nextAttemptNumber(ctx, task.ID)
	if err != nil {
		s.revertTaskToReady(ctx, task.ID, "attempt number lookup error")
		return store.TaskAttempt{}, fmt.Errorf("next attempt number for task %s: %w", task.ID, err)
	}

	attempt, err := s.store.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:            uuid.NewString(),
		TaskID:        task.ID,
		WorkerID:      worker.ID,
		AttemptNumber: nextNum,
		Status:        store.AttemptStatusRunning,
		StartedAt:     now,
		CreatedAt:     now,
	})
	if err != nil {
		s.revertTaskToReady(ctx, task.ID, "attempt creation error")
		return store.TaskAttempt{}, fmt.Errorf("create task attempt for task %s: %w", task.ID, err)
	}

	// Re-check pool availability and create claim rows inside a single DB
	// transaction so no concurrent assignment can over-subscribe a pool.
	claims := buildUsageClaims(step, pools)
	if len(claims) == 0 {
		return attempt, nil
	}

	if err := s.store.TryClaimSlots(ctx, attempt.ID, claims, now); err != nil {
		s.revertTaskToReady(ctx, task.ID, "usage claim error")
		if errors.Is(err, store.ErrUsageAtCapacity) {
			s.logger.DebugContext(
				ctx, "scheduler: usage pool at capacity — deferring assignment",
				slog.String("task_id", task.ID),
				slog.String("attempt_id", attempt.ID),
			)
			return store.TaskAttempt{}, errNoWorkerAvailable
		}
		return store.TaskAttempt{}, fmt.Errorf("claim usage slots for attempt %s: %w", attempt.ID, err)
	}
	return attempt, nil
}

// revertTaskToReady resets a task's status back to ready after a failed
// assignment step. Logs a warning if the revert itself fails.
func (s *Scheduler) revertTaskToReady(ctx context.Context, taskID, reason string) {
	if err := s.store.UpdateTaskStatus(ctx, taskID, store.TaskStatusReady); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: revert task assignment failed",
			slog.String("task_id", taskID),
			slog.String("during", reason),
			slog.Any("error", err),
		)
	}
}

// nextAttemptNumber returns the AttemptNumber to use for a new [store.TaskAttempt]
// on the given task. It is 1 for a task with no prior attempts, and
// latest.AttemptNumber+1 on each retry.
func (s *Scheduler) nextAttemptNumber(ctx context.Context, taskID string) (int, error) {
	latest, err := s.store.LatestTaskAttempt(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	return latest.AttemptNumber + 1, nil
}

// buildUsageClaims converts the step's usage pool requirements into
// [store.UsagePoolClaim] values ready for [store.UsageClaimStore.TryClaimSlots].
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
			ClaimID:       uuid.NewString(),
			PoolID:        pool.ID,
			PoolName:      pool.Name,
			MaxConcurrent: pool.MaxConcurrent,
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

// ── Usage release ────────────────────────────────────────────────────

// ReleaseTaskUsage releases all active usage-pool claims for the given task
// attempt. It is called when a task attempt transitions to a terminal state
// (succeeded, failed, or canceled), freeing the usage pool slots for other tasks.
//
// This method is safe to call with an empty attemptID — it returns nil without
// querying the store. It is idempotent: releasing an already-released claim
// is a no-op in the underlying SQL.
//
// The worker wire protocol calls this method when terminal task
// status messages arrive from workers.
func (s *Scheduler) ReleaseTaskUsage(ctx context.Context, attemptID string) error {
	if attemptID == "" {
		return nil
	}
	n, err := s.store.ReleaseAttemptClaims(ctx, attemptID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("release usage claims for attempt %s: %w", attemptID, err)
	}
	if n > 0 {
		s.logger.DebugContext(
			ctx, "scheduler: released usage-pool claims",
			slog.String("attempt_id", attemptID),
			slog.Int("count", n),
		)
	}
	return nil
}

// ── Worker NATS consumer ─────────────────────────────────────────────

// handleWorkerMessage is the JetStream message handler for both
// worker.register and worker.heartbeat subjects (both flow through the
// SQI_WORKER stream and its single durable consumer).
func (s *Scheduler) handleWorkerMessage(msg jetstream.Msg) {
	ctx := s.ctx
	subject := msg.Subject()

	switch subject {
	case bus.SubjectWorkerRegister:
		s.handleWorkerRegister(ctx, msg)
	case bus.SubjectWorkerHeartbeat:
		s.handleWorkerHeartbeat(ctx, msg)
	case bus.SubjectWorkerDeregister:
		s.handleWorkerDeregister(ctx, msg)
	default:
		s.logger.WarnContext(
			ctx, "scheduler: unexpected worker subject",
			slog.String("subject", subject),
		)
		s.ackMsg(ctx, msg)
	}
}

// RegisterMsg is the JSON payload workers publish to worker.register.
// It carries the worker's self-reported identity and capability data.
// A later protocol revision may add formal versioning; this struct matches
// the minimal information the server needs for task matching.
//
// IT IS A HAND-MAINTAINED DUPLICATE of [protocol.RegisterMsg], which the server
// deliberately does not import, and NOTHING BUT A TEST RELATES THE TWO. A json
// tag that differs on either side does not fail to compile: the field simply
// decodes to its zero value on every registration, forever, silently.
// TestRegisterMsg_WireFieldsSurviveTheDuplication marshals a fully-populated
// protocol.RegisterMsg, decodes it into THIS struct, and asserts every field
// arrives -- outer keys included. It was added after a reviewer renamed the
// expr_limits key on the protocol side and watched the entire test suite, the
// integration suite and make ci stay green while every worker in the farm
// reported as "not advertised".
type RegisterMsg struct {
	// WorkerID is the stable unique identifier for this worker instance.
	// Workers MUST use the same ID across restarts so that re-registration
	// updates the existing record rather than creating a duplicate.
	WorkerID string `json:"worker_id"`

	// FarmID is the farm this worker belongs to. Required.
	FarmID string `json:"farm_id"`

	// QueueID restricts the worker to a single queue if non-empty.
	// An empty value means the worker accepts tasks from any queue in FarmID.
	QueueID string `json:"queue_id,omitempty"`

	// Name is the worker's human-readable display label. Persisted so the UI
	// can distinguish multiple workers running on a single host.
	Name string `json:"name,omitempty"`

	// Hostname and IPAddress are the worker's network identity.
	Hostname  string `json:"hostname"`
	IPAddress string `json:"ip_address,omitempty"`

	// ComputeLocation is the named compute location this worker belongs to
	// (e.g. "onprem_linux", "cloud_aws_us_east"). Used for path translation
	// and task affinity.
	ComputeLocation string `json:"compute_location,omitempty"`

	// OS and OSVersion are the worker's operating system identity, used for
	// capability matching.
	OS        string `json:"os"`
	OSVersion string `json:"os_version,omitempty"`

	// WorkerVersion is the sqi-worker build version the worker self-reports,
	// distinct from the protocol Version field above.
	WorkerVersion string `json:"worker_version,omitempty"`

	// CPUCount and RAMMb are the worker's hardware capacity, used for
	// resource-aware scheduling in future phases.
	CPUCount int `json:"cpu_count,omitempty"`
	RAMMb    int `json:"ram_mb,omitempty"`

	// GPUInfo describes any GPU(s) installed on the worker host.
	// omitempty is omitted intentionally: it has no effect on struct fields.
	GPUInfo store.GPUInfo `json:"gpu_info"`

	// Tags holds arbitrary key/value capability tags the worker self-reports.
	// Job requirements reference these tags when specifying worker constraints.
	Tags map[string]string `json:"tags,omitempty"`

	// ExprLimits are the OpenJD EXPR evaluation caps the worker will enforce.
	// Decoded straight into the store type: the json tags on
	// protocol.ExprLimits and store.WorkerExprLimits are identical.
	// TestWorkerExprLimits_WireKeysMatchTheProtocol covers the FOUR INNER keys;
	// the outer expr_limits key is covered by
	// TestRegisterMsg_WireFieldsSurviveTheDuplication (see the type comment) --
	// two separate tests because a rename of either is independently silent.
	// Absent (a worker older than EXPR sub-project E4d Task 3) leaves it zero,
	// which exprcaps.go reads as the compiled-in defaults.
	ExprLimits store.WorkerExprLimits `json:"expr_limits,omitzero"`
}

// handleWorkerRegister processes a worker.register message:
// decodes the payload, upserts the worker in the store, and refreshes the
// WorkersTotal Prometheus gauge.
func (s *Scheduler) handleWorkerRegister(ctx context.Context, msg jetstream.Msg) {
	var m RegisterMsg
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: malformed worker.register message",
			slog.Any("error", err),
		)
		s.ackMsg(ctx, msg) // ack to discard; re-delivery cannot fix a bad payload
		return
	}
	if m.WorkerID == "" {
		s.logger.WarnContext(ctx, "scheduler: worker.register missing worker_id")
		s.ackMsg(ctx, msg)
		return
	}

	now := time.Now().UTC()
	w := store.Worker{
		ID:              m.WorkerID,
		FarmID:          m.FarmID,
		QueueID:         m.QueueID,
		Name:            m.Name,
		Hostname:        m.Hostname,
		IPAddress:       m.IPAddress,
		ComputeLocation: m.ComputeLocation,
		OS:              m.OS,
		OSVersion:       m.OSVersion,
		Version:         m.WorkerVersion,
		CPUCount:        m.CPUCount,
		RAMMb:           m.RAMMb,
		GPUInfo:         m.GPUInfo,
		Tags:            m.Tags,
		ExprLimits:      m.ExprLimits,
		Status:          store.WorkerStatusOnline,
		LastHeartbeatAt: &now,
	}

	if _, err := s.store.RegisterWorker(ctx, w); err != nil {
		s.logger.ErrorContext(
			ctx, "scheduler: persist worker registration failed",
			slog.String("worker_id", m.WorkerID),
			slog.Any("error", err),
		)
		s.nakMsg(ctx, msg)
		return
	}

	s.ensureComputeLocation(ctx, m.ComputeLocation)

	s.warnOnExprCapShortfall(ctx, m)

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
		Status:   string(store.WorkerStatusOnline),
	})
	s.refreshWorkerGauge(ctx)
	s.ackMsg(ctx, msg)
}

// warnOnExprCapShortfall logs, at most once per distinct shortfall per worker,
// that a registering worker's EXPR limits are below this server's.
//
// It is a DIAGNOSTIC, not the bound: the bound is exprCapsBlock refusing to
// lease EXPR work to this worker (exprcaps.go). It exists because that refusal
// is otherwise only visible on a task, and there may not be one yet.
//
// De-duplicated because the worker re-registers on every NATS reconnect
// (internal/worker/registration's Registrar.SetupReconnectHook), and a farm
// that never submits an EXPR template would
// otherwise accumulate a warning per reconnect about work it never runs. The
// key is the shortfall text itself, so a CHANGE (either side reconfigured) is
// reported again; the map is per-process, so a server restart also re-reports.
func (s *Scheduler) warnOnExprCapShortfall(ctx context.Context, m RegisterMsg) {
	short := exprCapShortfall(m.ExprLimits, s.cfg.ExprLimits)
	if short == "" {
		s.exprCapWarned.Delete(m.WorkerID) // recovered: report again if it recurs
		return
	}
	if prev, ok := s.exprCapWarned.Load(m.WorkerID); ok && prev == short {
		return
	}
	s.exprCapWarned.Store(m.WorkerID, short)
	s.logger.WarnContext(
		ctx, "scheduler: worker registered with EXPR limits tighter than this server's",
		slog.String("worker_id", m.WorkerID),
		slog.String("hostname", m.Hostname),
		slog.String("detail", short),
		slog.String("impact", "this worker is not offered EXPR jobs; it still runs everything else, "+
			"and a farm that submits no EXPR templates is unaffected"),
	)
}

// ensureComputeLocation idempotently registers a compute location reported by a
// worker. It is best-effort: errors are logged but never propagated, because the
// registry is a convenience catalog and must not fail worker registration. An
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
	if err != nil && !errors.Is(err, store.ErrConflict) {
		s.logger.WarnContext(ctx, "scheduler: auto-register compute location failed",
			slog.String("compute_location", name), slog.Any("error", err))
	}
}

// handleWorkerDeregister processes a worker.deregister message published by a
// worker on graceful shutdown. It marks the worker offline immediately so the
// scheduler stops dispatching new assignments to it rather than waiting for
// the heartbeat-timeout sweep.
func (s *Scheduler) handleWorkerDeregister(ctx context.Context, msg jetstream.Msg) {
	// DeregisterMsg mirrors protocol.DeregisterMsg; we decode only the
	// fields the server needs without importing the worker protocol package.
	var m struct {
		WorkerID string `json:"worker_id"`
		Reason   string `json:"reason,omitempty"`
	}
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: malformed worker.deregister message",
			slog.Any("error", err),
		)
		s.ackMsg(ctx, msg)
		return
	}
	if m.WorkerID == "" {
		s.logger.WarnContext(ctx, "scheduler: worker.deregister missing worker_id")
		s.ackMsg(ctx, msg)
		return
	}

	if err := s.store.UpdateWorkerStatus(ctx, m.WorkerID, store.WorkerStatusOffline); err != nil {
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
		// ack in all error cases — nacking would redeliver but neither a
		// not-found nor a store error is likely to resolve on retry, and
		// wedging the consumer would block all subsequent worker messages.
		s.ackMsg(ctx, msg)
		return
	}

	s.logger.InfoContext(
		ctx, "scheduler: worker deregistered",
		slog.String("worker_id", m.WorkerID),
		slog.String("reason", m.Reason),
	)

	// A gracefully-deregistered worker is now offline and the heartbeat sweep
	// (which only inspects workers still marked online) will never look at it
	// again. Reclaim its in-flight tasks here so they return to the ready queue
	// instead of being stranded in 'assigned'/'running'.
	s.reclaimOfflineWorkerTasks(ctx, m.WorkerID, "")

	s.notifier.NotifyWorker(ws.WorkerEvent{
		WorkerID: m.WorkerID,
		Status:   string(store.WorkerStatusOffline),
	})
	s.refreshWorkerGauge(ctx)
	s.ackMsg(ctx, msg)
}

// ── Heartbeat handler and sweep ──────────────────────────────────────

// HeartbeatMsg is the JSON payload workers publish to worker.heartbeat.
type HeartbeatMsg struct {
	WorkerID string    `json:"worker_id"`
	At       time.Time `json:"at"`
}

// handleWorkerHeartbeat processes a worker.heartbeat message by recording the
// current time as the worker's LastHeartbeatAt. If At is zero the server
// timestamp is used so clock skew on the worker side cannot cause false
// timeouts.
func (s *Scheduler) handleWorkerHeartbeat(ctx context.Context, msg jetstream.Msg) {
	var m HeartbeatMsg
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: malformed worker.heartbeat message",
			slog.Any("error", err),
		)
		s.ackMsg(ctx, msg)
		return
	}
	if m.WorkerID == "" {
		s.ackMsg(ctx, msg)
		return
	}

	at := m.At
	if at.IsZero() {
		at = time.Now().UTC()
	}

	if err := s.store.UpdateWorkerHeartbeat(ctx, m.WorkerID, at); err != nil {
		// ErrNotFound means the worker has not registered yet (e.g. heartbeat
		// arrived before registration replay completes). Nak so it is retried.
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
	s.ackMsg(ctx, msg)
}

// runHeartbeatSweep periodically finds workers whose last heartbeat is older
// than WorkerTimeout, marks them offline, and reclaims their in-flight tasks.
// On the same tick it reaps tasks stranded in 'assigned' on still-live workers,
// demotes stalled jobs, flags/clears ready tasks no online worker can satisfy
// ([sweepUnschedulable]), re-evaluates cross-job dependencies for any job
// stuck blocked ([sweepBlockedJobs] — the backstop for [Scheduler.ReconcileDependents]),
// and refreshes the queue-depth, idle-worker, and usage-claim Prometheus gauges.
func (s *Scheduler) runHeartbeatSweep(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.HeartbeatSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepStaleWorkers(ctx)
			s.sweepRetiredWorkers(ctx)
			s.sweepRetiredJobs(ctx)
			s.reapStaleAssignedTasks(ctx)
			s.demoteStalledJobs(ctx)
			s.sweepUnschedulable(ctx)
			if err := s.sweepBlockedJobs(ctx); err != nil {
				s.logger.ErrorContext(ctx, "scheduler: sweep blocked jobs failed", slog.Any("error", err))
			}
			// Refresh instrumentation gauges on the same tick so Prometheus
			// reflects current farm state without a dedicated metrics loop.
			s.refreshQueueDepthGauge(ctx)
			s.refreshIdleWorkerGauge(ctx)
			s.refreshUsageClaimGauge(ctx)
		}
	}
}

// demoteStalledJobs reconciles the JobStatusRunning invariant after a sweep or
// reap. A job whose last assigned/running task was just returned to the ready
// queue (worker died, or assignment reaped) is left marked 'running' even
// though nothing is in flight; this returns such jobs to 'pending' so their
// status reflects reality until a worker picks the work back up. Self-healing
// and idempotent: a job with work still in flight, or already terminal, is
// untouched.
func (s *Scheduler) demoteStalledJobs(ctx context.Context) {
	demoted, err := s.store.DemoteStalledJobs(ctx, time.Now().UTC())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: demote stalled jobs failed", slog.Any("error", err))
		}
		return
	}
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
// is still happily heartbeating (e.g. the assignment message expired from the
// work stream before the worker had a free slot to pull it), and nothing else
// in the system would ever recover it. For each reclaimed task it closes the
// provisional attempt and releases any usage-pool claims it held.
func (s *Scheduler) reapStaleAssignedTasks(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-s.cfg.AssignedTaskTimeout)
	reclaimed, err := s.store.ReclaimStaleAssignedTasks(ctx, cutoff)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: reclaim stale assigned tasks failed", slog.Any("error", err))
		}
		return
	}
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
		s.cleanupReapedAttempt(ctx, task, now)
		s.notifier.NotifyTask(ws.TaskEvent{
			JobID:     task.JobID,
			TaskID:    task.ID,
			Name:      task.Name,
			Status:    string(store.TaskStatusReady),
			UpdatedAt: now,
		})
		s.notifyQueueForJob(ctx, task.JobID)
	}
}

// cleanupReapedAttempt closes the provisional attempt for a reaped task and
// releases its usage-pool claims. Both steps are best-effort: the task is
// already back in the ready queue, so a cleanup failure leaks an attempt record
// or pool slot but never blocks rescheduling.
func (s *Scheduler) cleanupReapedAttempt(ctx context.Context, task store.Task, now time.Time) {
	attempt, err := s.store.LatestTaskAttempt(ctx, task.ID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: reap cleanup: latest attempt lookup failed",
			slog.String("task_id", task.ID),
			slog.Any("error", err),
		)
		return
	}
	// Only the open provisional attempt needs closing; a terminal attempt is
	// already accounted for.
	if attempt.Status == store.AttemptStatusRunning {
		closed := attempt
		closed.Status = store.AttemptStatusFailed
		closed.EndedAt = &now
		if _, err := s.store.UpdateTaskAttempt(ctx, closed); err != nil {
			s.logger.WarnContext(
				ctx, "scheduler: reap cleanup: close attempt failed",
				slog.String("task_id", task.ID),
				slog.String("attempt_id", attempt.ID),
				slog.Any("error", err),
			)
		}
	}
	if err := s.ReleaseTaskUsage(ctx, attempt.ID); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: reap cleanup: release usage failed",
			slog.String("task_id", task.ID),
			slog.String("attempt_id", attempt.ID),
			slog.Any("error", err),
		)
	}
}

// sweepStaleWorkers finds workers whose heartbeat has expired, marks them
// offline, reclaims their assigned/running tasks, and refreshes the
// WorkersTotal gauge.
func (s *Scheduler) sweepStaleWorkers(ctx context.Context) {
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
		// Mark the worker offline.
		if err := s.store.UpdateWorkerStatus(ctx, w.ID, store.WorkerStatusOffline); err != nil {
			s.logger.WarnContext(
				ctx, "scheduler: mark worker offline failed",
				slog.String("worker_id", w.ID),
				slog.Any("error", err),
			)
			continue
		}
		s.notifier.NotifyWorker(ws.WorkerEvent{
			WorkerID: w.ID,
			Name:     w.Name,
			Hostname: w.Hostname,
			FarmID:   w.FarmID,
			Status:   string(store.WorkerStatusOffline),
		})

		// Close out running attempts and return the worker's in-flight tasks to
		// the ready queue so they can be reassigned.
		s.reclaimOfflineWorkerTasks(ctx, w.ID, w.Hostname)
	}

	s.refreshWorkerGauge(ctx)
}

// sweepRetiredWorkers hard-deletes workers that have been offline longer than
// OfflineWorkerRetention, bounding the worker table's growth on farms with
// ephemeral nodes. Offline workers already had their in-flight tasks reclaimed
// when they went offline, so no reclaim is needed here. Disabled and online
// workers are never auto-removed. A non-positive retention disables the sweep.
func (s *Scheduler) sweepRetiredWorkers(ctx context.Context) {
	if s.cfg.OfflineWorkerRetention <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.cfg.OfflineWorkerRetention)
	removed, err := s.store.DeleteOfflineWorkersBefore(ctx, cutoff)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: retention sweep failed", slog.Any("error", err))
		}
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
		s.notifier.NotifyWorker(ws.WorkerEvent{
			WorkerID: w.ID,
			Name:     w.Name,
			Hostname: w.Hostname,
			FarmID:   w.FarmID,
			Status:   ws.WorkerStatusRemoved,
		})
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
	removed, err := s.store.DeleteTerminalJobsBefore(ctx, cutoff, s.cfg.JobRetentionIncludeFailed)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: job retention sweep failed", slog.Any("error", err))
		}
		return
	}
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

// reclaimOfflineWorkerTasks closes any running attempt records for workerID and
// returns its assigned/running tasks to the ready queue. It is shared by the
// heartbeat sweep and the graceful-deregister handler: both mark a worker
// offline and must hand its in-flight work back to the scheduler, otherwise the
// tasks are orphaned in 'assigned'/'running' forever (the heartbeat sweep only
// considers workers still marked online, so it cannot recover them afterwards).
func (s *Scheduler) reclaimOfflineWorkerTasks(ctx context.Context, workerID, hostname string) {
	// Close out any running attempt records before the task assignment is
	// cleared by ReclaimWorkerTasks. The subquery in TerminateWorkerAttempts
	// joins on assigned_worker_id, which is still set at this point.
	now := time.Now().UTC()
	nAttempts, err := s.store.TerminateWorkerAttempts(ctx, workerID, store.AttemptStatusFailed, now)
	if err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: terminate worker attempts failed",
			slog.String("worker_id", workerID),
			slog.Any("error", err),
		)
		// Non-fatal: continue to reclaim tasks so the farm keeps running.
	} else if nAttempts > 0 {
		s.logger.InfoContext(
			ctx, "scheduler: closed running attempts for offline worker",
			slog.String("worker_id", workerID),
			slog.String("hostname", hostname),
			slog.Int("attempts_closed", nAttempts),
		)
	}

	// Reclaim tasks that were assigned to or running on the now-offline worker.
	n, err := s.store.ReclaimWorkerTasks(ctx, workerID)
	switch {
	case err != nil:
		s.logger.WarnContext(
			ctx, "scheduler: reclaim worker tasks failed",
			slog.String("worker_id", workerID),
			slog.Any("error", err),
		)
	case n > 0:
		s.logger.InfoContext(
			ctx, "scheduler: reclaimed tasks from offline worker",
			slog.String("worker_id", workerID),
			slog.String("hostname", hostname),
			slog.Int("tasks_reclaimed", n),
		)
		// Reclaimed tasks are back to ready but we have no jobIDs to scope a
		// per-queue wake; broadcast so parked workers re-lease promptly.
		s.waiters.notifyAll()
	default:
		s.logger.InfoContext(
			ctx, "scheduler: worker marked offline (no tasks to reclaim)",
			slog.String("worker_id", workerID),
			slog.String("hostname", hostname),
		)
	}
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

// notifyQueueForJob wakes any parked lease waiters on the job's queue (and any
// queue-unaffiliated workers). Best-effort: a missed wake is self-correcting
// because the worker's outstanding lease request re-issues after leaseHoldTimeout.
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

		// Use Total from the page to avoid fetching all rows.
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
// gauge, and wakes lease waiters on every queue with leasable work — the
// restart-safe backstop for scheduleRetryWake's in-process backoff timers.
//
// Called on every heartbeat-sweep tick so the gauge stays current.  Errors are
// logged at WARN level and do not abort the sweep.
func (s *Scheduler) refreshQueueDepthGauge(ctx context.Context) {
	counts, err := s.store.CountReadyTasksByQueue(ctx, s.cfg.FarmID, time.Now().UTC())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: refresh queue depth gauge failed", slog.Any("error", err))
		}
		return
	}
	for queueID, n := range counts {
		s.metrics.SchedulerQueueDepth.WithLabelValues(queueID).Set(float64(n))
		if n > 0 {
			s.WakeQueue(queueID)
		}
	}
}

// refreshUsageClaimGauge queries the store for all configured usage pools and
// the active claim count for each, then updates the UsageActiveClaims Prometheus
// gauge.
//
// Called on every heartbeat-sweep tick. Errors are non-fatal — a stale gauge is
// preferable to aborting the sweep.
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

// ackMsg acknowledges msg and logs a warning if the ack itself fails.
// Ack failures are rare in the embedded-NATS setup but should not be silently
// discarded, as they can indicate a connection problem.
func (s *Scheduler) ackMsg(ctx context.Context, msg jetstream.Msg) {
	if err := msg.Ack(); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: ack message failed",
			slog.String("subject", msg.Subject()),
			slog.Any("error", err),
		)
	}
}

// nakMsg negatively-acknowledges msg (requesting redelivery) and logs a
// warning if the nak itself fails.
func (s *Scheduler) nakMsg(ctx context.Context, msg jetstream.Msg) {
	if err := msg.Nak(); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: nak message failed",
			slog.String("subject", msg.Subject()),
			slog.Any("error", err),
		)
	}
}
