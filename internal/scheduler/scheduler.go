// SPDX-License-Identifier: AGPL-3.0-only

// Package scheduler implements the sqi-server assignment loop and worker
// registry.
//
// The [Scheduler] is the authoritative component for deciding which task runs
// on which worker. It owns three concurrent loops:
//
//  1. Assignment loop (task 46): a goroutine pool that periodically polls the
//     store for ready tasks and, for each one, selects an eligible online
//     worker, calls [store.TaskStore.AssignTask], and publishes the assignment
//     payload to the appropriate NATS work-assignment subject.
//
//  2. Worker registry (task 47): a NATS push-consumer that processes
//     worker.register messages, persisting capability data via
//     [store.WorkerStore.RegisterWorker] and keeping the WorkersTotal
//     Prometheus gauge current.
//
//  3. Heartbeat sweep (task 48): a NATS push-consumer that updates each
//     worker's LastHeartbeatAt timestamp on receipt of a worker.heartbeat
//     message, paired with a periodic timer that calls
//     [store.WorkerStore.ListStaleWorkers], marks each stale worker offline,
//     and calls [store.TaskStore.ReclaimWorkerTasks] to return their
//     in-flight tasks to the ready queue.
//
// Task 49 introduced full priority ordering into the ready-task query
// ([store.TaskStore.ListReadyTasks]): tasks arrive pre-sorted by job priority,
// job submission time, step order, and task creation time.
//
// Task 50 added capability-tag matching, compute-location affinity, and
// queue/farm assignment filtering via [WorkerEligible].
//
// Task 51 added per-queue and per-farm maximum concurrent task limits evaluated
// inside [policyGate] before any worker is selected.
//
// Task 52 added atomic license-slot claiming ([store.LicenseCheckoutStore.TryClaimLicenseSlots])
// after a worker is selected and a provisional [store.TaskAttempt] is created.
// If the claim fails because the pool is saturated the assignment is rolled
// back and the task remains ready for the next dispatch tick.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
)

// DefaultConfig returns a [Config] with conservative production-safe defaults.
func DefaultConfig() Config {
	return Config{
		FarmID:                 "",
		AssignInterval:         time.Second,
		AssignBatchSize:        50,
		AssignWorkers:          4,
		WorkerTimeout:          30 * time.Second,
		HeartbeatSweepInterval: 15 * time.Second,
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
}

// Scheduler owns the assignment loop, worker registry, and heartbeat sweep.
// Create it with [New] and drive it with [Run].
type Scheduler struct {
	cfg     Config
	store   store.Store
	bus     *bus.Client
	metrics *metrics.Metrics
	logger  *slog.Logger

	// taskCh carries ready tasks from the dispatch goroutine to the assignment
	// worker pool. Sized to AssignBatchSize so a full batch can be queued
	// without blocking the dispatch loop.
	taskCh chan store.Task

	// wg tracks all internal goroutines so [Run] can wait for clean exit.
	wg sync.WaitGroup

	// cancel is called to stop all internal goroutines; set during [Run].
	cancel context.CancelFunc
}

// New creates a Scheduler. Call [Run] to start its goroutines.
func New(cfg Config, st store.Store, busClient *bus.Client, m *metrics.Metrics, logger *slog.Logger) *Scheduler {
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
	return &Scheduler{
		cfg:     cfg,
		store:   st,
		bus:     busClient,
		metrics: m,
		logger:  logger,
		taskCh:  make(chan store.Task, cfg.AssignBatchSize),
	}
}

// Run starts all scheduler goroutines and blocks until ctx is canceled.
// It returns after all goroutines have exited cleanly.
func (s *Scheduler) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()

	s.logger.InfoContext(
		ctx, "scheduler: starting",
		slog.String("farm_id", s.cfg.FarmID),
		slog.Duration("assign_interval", s.cfg.AssignInterval),
		slog.Int("assign_workers", s.cfg.AssignWorkers),
		slog.Duration("worker_timeout", s.cfg.WorkerTimeout),
		slog.Duration("heartbeat_sweep_interval", s.cfg.HeartbeatSweepInterval),
	)

	// ── Task 47 + 48: worker NATS consumer ────────────────────────────────
	// A single JetStream push-consumer delivers both worker.register and
	// worker.heartbeat messages. The handler dispatches by subject.
	_, err := s.bus.ConsumeWorker(ctx, s.handleWorkerMessage)
	if err != nil {
		return fmt.Errorf("scheduler: start worker consumer: %w", err)
	}
	s.logger.InfoContext(ctx, "scheduler: worker consumer started")

	// ── Task 46: assignment worker pool ───────────────────────────────────
	for i := range s.cfg.AssignWorkers {
		s.wg.Go(func() {
			s.runAssignWorker(ctx, i)
		})
	}

	// ── Task 46: dispatch loop ─────────────────────────────────────────────
	s.wg.Go(func() {
		s.runDispatchLoop(ctx)
	})

	// ── Task 48: heartbeat sweep ───────────────────────────────────────────
	s.wg.Go(func() {
		s.runHeartbeatSweep(ctx)
	})

	// Seed WorkersTotal gauge with current DB state so it is non-zero on
	// startup even if no NATS registration messages have been seen yet.
	s.refreshWorkerGauge(ctx)

	// Block until canceled, then wait for goroutines.
	<-ctx.Done()
	s.logger.InfoContext(ctx, "scheduler: shutdown signaled — draining goroutines")

	// Close taskCh so assignment workers exit their range loop.
	close(s.taskCh)
	s.wg.Wait()

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

// ── Task 46: dispatch loop ────────────────────────────────────────────────────

// runDispatchLoop ticks on AssignInterval, fetches a batch of ready tasks from
// the store, and pushes each task onto taskCh for the worker pool to process.
func (s *Scheduler) runDispatchLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.AssignInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.dispatchBatch(ctx)
		}
	}
}

// dispatchBatch fetches ready tasks and fans them out to the assignment workers.
func (s *Scheduler) dispatchBatch(ctx context.Context) {
	tasks, err := s.store.ListReadyTasks(ctx, s.cfg.FarmID, s.cfg.AssignBatchSize)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: list ready tasks failed", slog.Any("error", err))
		}
		return
	}
	if len(tasks) == 0 {
		return
	}
	s.logger.DebugContext(ctx, "scheduler: dispatching ready tasks", slog.Int("count", len(tasks)))

	for _, t := range tasks {
		select {
		case s.taskCh <- t:
		case <-ctx.Done():
			return
		}
	}
}

// ── Task 46: assignment worker pool ───────────────────────────────────────────

// runAssignWorker pulls tasks from taskCh and attempts to assign each one to
// an available online worker.
//
// Tasks arrive pre-sorted by [store.TaskStore.ListReadyTasks]: highest job
// priority first, then earlier job submission time, then lower step order,
// then task creation time (task 49). [tryAssign] applies capability-tag
// matching and compute-location affinity (task 50), queue/farm concurrency
// policy (task 51), and atomic license claiming (task 52).
func (s *Scheduler) runAssignWorker(ctx context.Context, id int) {
	s.logger.DebugContext(ctx, "scheduler: assignment worker started", slog.Int("worker_id", id))

	for task := range s.taskCh {
		if ctx.Err() != nil {
			return
		}
		if err := s.tryAssign(ctx, task); err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, errNoWorkerAvailable) {
				s.logger.WarnContext(
					ctx, "scheduler: assignment failed",
					slog.String("task_id", task.ID),
					slog.Any("error", err),
				)
			}
		}
	}
	s.logger.DebugContext(ctx, "scheduler: assignment worker stopped", slog.Int("worker_id", id))
}

// errNoWorkerAvailable is returned by tryAssign when no eligible online worker
// exists. It is not logged as a warning — the task simply stays ready until
// the next dispatch tick.
var errNoWorkerAvailable = errors.New("no worker available")

// tryAssign selects an eligible online worker for task using full capability
// matching (task 50), policy gates (task 51), and atomic license claiming
// (task 52), then updates the store and publishes the assignment to NATS.
func (s *Scheduler) tryAssign(ctx context.Context, task store.Task) error {
	job, err := s.store.GetJob(ctx, task.JobID)
	if err != nil {
		return fmt.Errorf("get job %s: %w", task.JobID, err)
	}

	step, err := s.store.GetStep(ctx, task.StepID)
	if err != nil {
		return fmt.Errorf("get step %s: %w", task.StepID, err)
	}

	// ── Task 51: queue and farm policy gate ───────────────────────────────
	queue, err := s.store.GetQueue(ctx, job.QueueID)
	if err != nil {
		return fmt.Errorf("get queue %s: %w", job.QueueID, err)
	}
	farm, err := s.store.GetFarm(ctx, job.FarmID)
	if err != nil {
		return fmt.Errorf("get farm %s: %w", job.FarmID, err)
	}

	if err := policyGate(ctx, s.store, job, queue, farm); err != nil {
		if errors.Is(err, errPolicyBlocked) {
			s.logger.DebugContext(
				ctx, "scheduler: assignment deferred by policy",
				slog.String("task_id", task.ID),
				slog.String("queue_id", job.QueueID),
				slog.String("farm_id", job.FarmID),
				slog.String("reason", err.Error()),
			)
			return errNoWorkerAvailable // silent; retry on next tick
		}
		return err
	}

	// Build the license pool context needed by the matcher (task 50 pre-check).
	pools, activeCounts, err := s.buildLicenseContext(ctx, step)
	if err != nil {
		return fmt.Errorf("build license context for step %s: %w", step.ID, err)
	}

	// Find the first eligible online worker using capability matching.
	worker, err := s.pickWorker(ctx, job, step, pools, activeCounts)
	if err != nil {
		return err // includes errNoWorkerAvailable
	}

	now := time.Now().UTC()
	if err := s.store.AssignTask(ctx, task.ID, worker.ID, now); err != nil {
		return fmt.Errorf("assign task %s to worker %s: %w", task.ID, worker.ID, err)
	}

	// ── Task 52: provisional attempt + atomic license claim ───────────────
	attempt, err := s.createAttemptAndClaimLicenses(ctx, task, worker, step, pools, now)
	if err != nil {
		return err // already reverted task status inside helper
	}

	// Publish the assignment to NATS so the worker can pull it.
	payload, err := buildAssignPayload(task, worker)
	if err != nil {
		return fmt.Errorf("build assign payload: %w", err)
	}
	if err := s.bus.PublishWorkAssign(ctx, job.QueueID, payload); err != nil {
		// The task is already marked assigned in the store; the worker will
		// pick it up when the heartbeat sweep eventually requeues it if the
		// NATS publish fails. Log and continue.
		s.logger.WarnContext(
			ctx, "scheduler: publish work assign failed",
			slog.String("task_id", task.ID),
			slog.String("worker_id", worker.ID),
			slog.Any("error", err),
		)
		return err
	}

	s.logger.InfoContext(
		ctx, "scheduler: task assigned",
		slog.String("task_id", task.ID),
		slog.String("worker_id", worker.ID),
		slog.String("queue_id", job.QueueID),
		slog.String("attempt_id", attempt.ID),
	)
	return nil
}

// createAttemptAndClaimLicenses creates a provisional [store.TaskAttempt] for
// the assignment and atomically claims any required license slots (task 52).
//
// If either operation fails the task's status is reverted to
// [store.TaskStatusReady] so it is re-queued on the next dispatch tick.
// [errNoWorkerAvailable] is returned when a license pool is at capacity so
// the caller skips logging a warning.
func (s *Scheduler) createAttemptAndClaimLicenses(
	ctx context.Context,
	task store.Task,
	worker store.Worker,
	step store.Step,
	pools map[string]store.LicensePool,
	now time.Time,
) (store.TaskAttempt, error) {
	// The attempt record must exist before license checkouts can be created
	// (FK constraint). Task 53 adds full retry management; here we create
	// attempt #1 at assignment time with AttemptStatusRunning.
	attempt, err := s.store.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:            uuid.NewString(),
		TaskID:        task.ID,
		WorkerID:      worker.ID,
		AttemptNumber: 1,
		Status:        store.AttemptStatusRunning,
		StartedAt:     now,
		CreatedAt:     now,
	})
	if err != nil {
		s.revertTaskToReady(ctx, task.ID, "attempt creation error")
		return store.TaskAttempt{}, fmt.Errorf("create task attempt for task %s: %w", task.ID, err)
	}

	// Re-check pool availability and create checkout rows inside a single DB
	// transaction so no concurrent assignment can over-subscribe a pool.
	claims := buildLicenseClaims(step, pools)
	if len(claims) == 0 {
		return attempt, nil
	}

	if err := s.store.TryClaimLicenseSlots(ctx, attempt.ID, claims, now); err != nil {
		s.revertTaskToReady(ctx, task.ID, "license claim error")
		if errors.Is(err, store.ErrLicenseAtCapacity) {
			s.logger.DebugContext(
				ctx, "scheduler: license pool at capacity — deferring assignment",
				slog.String("task_id", task.ID),
				slog.String("attempt_id", attempt.ID),
			)
			return store.TaskAttempt{}, errNoWorkerAvailable
		}
		return store.TaskAttempt{}, fmt.Errorf("claim license slots for attempt %s: %w", attempt.ID, err)
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

// buildLicenseClaims converts the step's license pool requirements into
// [store.LicensePoolClaim] values ready for [store.LicenseCheckoutStore.TryClaimLicenseSlots].
// Each claim gets a fresh UUID as its checkout ID.
// Pools not found in the pools map are skipped (the matcher already rejected
// workers when the pool was missing, so this path is unreachable in practice).
func buildLicenseClaims(step store.Step, pools map[string]store.LicensePool) []store.LicensePoolClaim {
	if step.HostRequirements == nil || len(step.HostRequirements.LicensePools) == 0 {
		return nil
	}
	claims := make([]store.LicensePoolClaim, 0, len(step.HostRequirements.LicensePools))
	for _, name := range step.HostRequirements.LicensePools {
		pool, ok := pools[name]
		if !ok {
			continue
		}
		claims = append(claims, store.LicensePoolClaim{
			CheckoutID:    uuid.NewString(),
			PoolID:        pool.ID,
			PoolName:      pool.Name,
			MaxConcurrent: pool.MaxConcurrent,
		})
	}
	return claims
}

// buildLicenseContext fetches all configured license pools and the current
// active checkout count for each pool required by step.
// Both return values are safe to pass to [WorkerEligible] when step has no
// license requirements.
func (s *Scheduler) buildLicenseContext(
	ctx context.Context,
	step store.Step,
) (pools map[string]store.LicensePool, activeCounts map[string]int, err error) {
	pools = make(map[string]store.LicensePool)
	activeCounts = make(map[string]int)

	if step.HostRequirements == nil || len(step.HostRequirements.LicensePools) == 0 {
		return pools, activeCounts, nil
	}

	// Fetch all pools once and key them by name.
	allPools, err := s.store.ListLicensePools(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list license pools: %w", err)
	}
	for _, p := range allPools {
		pools[p.Name] = p
	}

	// Fetch active checkout count only for pools the step actually requires.
	for _, name := range step.HostRequirements.LicensePools {
		pool, ok := pools[name]
		if !ok {
			// Pool not found; leave activeCounts[name] as zero so the matcher
			// rejects due to missing pool (capacity = 0).
			continue
		}
		count, err := s.store.ActiveCheckoutCount(ctx, pool.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("active checkout count for pool %q: %w", name, err)
		}
		activeCounts[name] = count
	}

	return pools, activeCounts, nil
}

// matchCandidateLimit is the maximum number of online workers fetched per
// assignment attempt. Large enough to find an eligible worker in most farms
// without loading the entire worker table.
const matchCandidateLimit = 50

// pickWorker returns the first online worker in job's farm that passes
// [WorkerEligible] checks for the given step and license pool state.
//
// Filtering strategy:
//   - SQL-level pre-filter: farm, status=online, optional compute-location.
//   - Go-level post-filter: queue affinity, capability tags, license pools.
//
// If no eligible worker is found within [matchCandidateLimit] candidates,
// [errNoWorkerAvailable] is returned and the task remains ready for the next
// dispatch tick.
func (s *Scheduler) pickWorker(
	ctx context.Context,
	job store.Job,
	step store.Step,
	pools map[string]store.LicensePool,
	activeCounts map[string]int,
) (store.Worker, error) {
	opts := store.ListWorkersOptions{
		FarmID: job.FarmID,
		Status: store.WorkerStatusOnline,
		Pagination: store.Pagination{
			Limit:  matchCandidateLimit,
			Offset: 0,
		},
	}

	// Apply SQL-level compute-location pre-filter when the step requires one.
	// This narrows the candidate set cheaply before in-memory matching.
	if step.ComputeLocation != "" {
		opts.ComputeLocation = step.ComputeLocation
	}

	opts.Pagination.Validate() //nolint:errcheck // Validate only clamps values; it never returns a non-nil error

	page, err := s.store.ListWorkers(ctx, opts)
	if err != nil {
		return store.Worker{}, fmt.Errorf("list workers: %w", err)
	}

	for _, w := range page.Items {
		reason, eligible := WorkerEligibleWithReason(w, job, step, pools, activeCounts)
		if eligible {
			return w, nil
		}
		s.logger.DebugContext(
			ctx, "scheduler: worker ineligible",
			slog.String("worker_id", w.ID),
			slog.String("hostname", w.Hostname),
			slog.String("task_step_id", step.ID),
			slog.String("reason", reason),
		)
	}

	return store.Worker{}, errNoWorkerAvailable
}

// assignPayload is the JSON message published to work.assign.<queueID>.
// Task 56 will introduce a fully versioned protocol; this interim format
// carries the minimum information a worker needs to begin execution.
type assignPayload struct {
	TaskID     string            `json:"task_id"`
	JobID      string            `json:"job_id"`
	StepID     string            `json:"step_id"`
	TaskName   string            `json:"task_name"`
	Parameters map[string]string `json:"parameters"`
	WorkerID   string            `json:"worker_id"`
	AssignedAt time.Time         `json:"assigned_at"`
}

func buildAssignPayload(task store.Task, worker store.Worker) ([]byte, error) {
	p := assignPayload{
		TaskID:     task.ID,
		JobID:      task.JobID,
		StepID:     task.StepID,
		TaskName:   task.Name,
		Parameters: task.Parameters,
		WorkerID:   worker.ID,
		AssignedAt: time.Now().UTC(),
	}
	return json.Marshal(p)
}

// ── Task 52: license release ──────────────────────────────────────────────────

// ReleaseTaskLicenses releases all active license checkouts for the given task
// attempt. It is called when a task attempt transitions to a terminal state
// (succeeded, failed, or canceled), freeing the license slots for other tasks.
//
// This method is safe to call with an empty attemptID — it returns nil without
// querying the store. It is idempotent: releasing an already-released checkout
// is a no-op in the underlying SQL.
//
// Task 56–59 (worker wire protocol) will call this method when terminal task
// status messages arrive from workers.
func (s *Scheduler) ReleaseTaskLicenses(ctx context.Context, attemptID string) error {
	if attemptID == "" {
		return nil
	}
	n, err := s.store.ReleaseAttemptCheckouts(ctx, attemptID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("release licenses for attempt %s: %w", attemptID, err)
	}
	if n > 0 {
		s.logger.DebugContext(
			ctx, "scheduler: released license checkouts",
			slog.String("attempt_id", attemptID),
			slog.Int("count", n),
		)
	}
	return nil
}

// ── Task 47: worker NATS consumer ─────────────────────────────────────────────

// handleWorkerMessage is the JetStream message handler for both
// worker.register and worker.heartbeat subjects (both flow through the
// SQI_WORKER stream and its single durable consumer).
func (s *Scheduler) handleWorkerMessage(msg jetstream.Msg) {
	ctx := context.Background()
	subject := msg.Subject()

	switch subject {
	case bus.SubjectWorkerRegister:
		s.handleWorkerRegister(ctx, msg)
	case bus.SubjectWorkerHeartbeat:
		s.handleWorkerHeartbeat(ctx, msg)
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
// Task 56 will introduce a formally versioned protocol; this struct matches
// the minimal information the server needs for task matching in tasks 46–52.
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

	// Hostname and IPAddress are the worker's network identity.
	Hostname  string `json:"hostname"`
	IPAddress string `json:"ip_address,omitempty"`

	// ComputeLocation is the named compute location this worker belongs to
	// (e.g. "onprem_linux", "cloud_aws_us_east"). Used for path translation
	// and task affinity (tasks 60–62).
	ComputeLocation string `json:"compute_location,omitempty"`

	// OS and OSVersion are the worker's operating system identity, used for
	// capability matching (tasks 49–50).
	OS        string `json:"os"`
	OSVersion string `json:"os_version,omitempty"`

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
		Hostname:        m.Hostname,
		IPAddress:       m.IPAddress,
		ComputeLocation: m.ComputeLocation,
		OS:              m.OS,
		OSVersion:       m.OSVersion,
		CPUCount:        m.CPUCount,
		RAMMb:           m.RAMMb,
		GPUInfo:         m.GPUInfo,
		Tags:            m.Tags,
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

	s.logger.InfoContext(
		ctx, "scheduler: worker registered",
		slog.String("worker_id", m.WorkerID),
		slog.String("hostname", m.Hostname),
		slog.String("os", m.OS),
		slog.String("farm_id", m.FarmID),
	)
	s.refreshWorkerGauge(ctx)
	s.ackMsg(ctx, msg)
}

// ── Task 48: heartbeat handler and sweep ──────────────────────────────────────

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
func (s *Scheduler) runHeartbeatSweep(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.HeartbeatSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepStaleWorkers(ctx)
		}
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

		// Reclaim tasks that were assigned to or running on the now-offline worker.
		n, err := s.store.ReclaimWorkerTasks(ctx, w.ID)
		switch {
		case err != nil:
			s.logger.WarnContext(
				ctx, "scheduler: reclaim worker tasks failed",
				slog.String("worker_id", w.ID),
				slog.Any("error", err),
			)
		case n > 0:
			s.logger.InfoContext(
				ctx, "scheduler: reclaimed tasks from offline worker",
				slog.String("worker_id", w.ID),
				slog.String("hostname", w.Hostname),
				slog.Int("tasks_reclaimed", n),
			)
		default:
			s.logger.InfoContext(
				ctx, "scheduler: worker marked offline (no tasks to reclaim)",
				slog.String("worker_id", w.ID),
				slog.String("hostname", w.Hostname),
			)
		}
	}

	s.refreshWorkerGauge(ctx)
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
