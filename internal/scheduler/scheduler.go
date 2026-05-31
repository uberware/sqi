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
// Tasks 49–52 extend the assignment loop with capability matching, policy
// evaluation, compute-location affinity, and license gating.  The loop
// skeleton here is intentionally simple: it picks the first available online
// worker for each ready task so that the end-to-end path can be exercised
// before the full matching logic lands.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
// NOTE: This is the simplified skeleton for tasks 46–48. Tasks 49–52 add
// capability matching, compute-location affinity, license gating, and queue
// policy evaluation. For now the loop picks the first online worker that is
// not currently assigned a task.
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

// tryAssign selects an online worker for task, updates the store, and
// publishes the assignment to NATS. It is intentionally simple: tasks 49–52
// replace the worker selection with full capability matching.
func (s *Scheduler) tryAssign(ctx context.Context, task store.Task) error {
	// Derive the farm from the task's job. We need the queue to determine the
	// NATS subject. Fetch the task's job to get its queue.
	job, err := s.store.GetJob(ctx, task.JobID)
	if err != nil {
		return fmt.Errorf("get job %s: %w", task.JobID, err)
	}

	// Find an online worker — simple first-available selection (tasks 49–52
	// will add full capability matching here).
	worker, err := s.pickWorker(ctx, job.FarmID, job.QueueID)
	if err != nil {
		return err // includes errNoWorkerAvailable
	}

	now := time.Now().UTC()
	if err := s.store.AssignTask(ctx, task.ID, worker.ID, now); err != nil {
		return fmt.Errorf("assign task %s to worker %s: %w", task.ID, worker.ID, err)
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
	)
	return nil
}

// pickWorker returns the first online worker in the farm/queue. Tasks 49–52
// will replace this with full capability-tag, compute-location, and license
// matching.
func (s *Scheduler) pickWorker(ctx context.Context, farmID, queueID string) (store.Worker, error) {
	opts := store.ListWorkersOptions{
		FarmID:  farmID,
		QueueID: queueID,
		Status:  store.WorkerStatusOnline,
		Pagination: store.Pagination{
			Limit:  1,
			Offset: 0,
		},
	}
	opts.Pagination.Validate() //nolint:errcheck // Validate only clamps values; it never returns a non-nil error

	page, err := s.store.ListWorkers(ctx, opts)
	if err != nil {
		return store.Worker{}, fmt.Errorf("list workers: %w", err)
	}
	if len(page.Items) == 0 {
		// Try without queue affinity — accept any online worker in the farm.
		opts.QueueID = ""
		page, err = s.store.ListWorkers(ctx, opts)
		if err != nil {
			return store.Worker{}, fmt.Errorf("list workers (no queue filter): %w", err)
		}
		if len(page.Items) == 0 {
			return store.Worker{}, errNoWorkerAvailable
		}
	}
	return page.Items[0], nil
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
