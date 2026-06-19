// SPDX-License-Identifier: AGPL-3.0-or-later

// Package executor implements the bare-metal task executor for sqi-worker.
//
// An [Executor] starts OS processes via [os/exec] for each assigned task,
// captures their stdout and stderr line-by-line, manages per-task execution
// timeouts and cancellation per the OpenJD Action.cancelation method (TERMINATE
// = immediate SIGKILL, the spec default; NOTIFY_THEN_TERMINATE = SIGTERM then
// SIGKILL after the notify period), and publishes task-status messages back to
// sqi-server via NATS.
//
// # Interfaces
//
// Executor implements both [pull.TaskDispatcher] and [pull.StateSource] so it
// can be wired directly into the pull loop.  It also satisfies
// [heartbeat.StateSource] by exposing [Executor.ActiveTaskCount],
// [Executor.ActiveTaskIDs], and [Executor.LastAssignmentAt].
//
// # Concurrency
//
// The server gates how many tasks a worker receives via CPU-core accounting;
// the worker runs whatever it is leased without an additional local cap.
// Multiple leases are dispatched in separate goroutines and run concurrently.
//
// # Output handling
//
// The [OutputHandler] interface abstracts what happens with each line of
// process output. The log-chunk publisher and OpenJD progress
// line parser will implement this interface; until then the
// [LogOutput] implementation forwards lines to the structured logger.
//
// # Root-user check
//
// [CheckRootUser] should be called before creating an Executor.  It returns
// an error if the worker process is running as root on Linux/macOS and
// [Config.AllowRoot] is false (see docs/worker-configuration.md, "worker.allow_root").
package executor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// ── Interfaces ────────────────────────────────────────────────────────────────

// OutputHandler processes lines of output emitted by a running task process.
// Implementations include the log-chunk publisher and the
// OpenJD progress-line parser. An OutputHandler must be safe
// for concurrent use from multiple goroutines (multiple tasks may emit output
// simultaneously).
type OutputHandler interface {
	// HandleLine is called for each line read from the task process stdout or
	// stderr.  stream is "stdout" or "stderr".  The line does not include a
	// trailing newline.
	HandleLine(ctx context.Context, taskID, attemptID, sessionID, stream, line string)
}

// LogFlusher is an optional interface that an [OutputHandler] may implement
// to flush remaining buffered log output for a specific task attempt before
// the terminal status message is published.
//
// If the configured OutputHandler also implements LogFlusher, the executor
// calls FlushLogs immediately after the task process exits and before
// publishing the terminal "succeeded", "failed", or "canceled" status.
// This guarantees complete log delivery to the server before the task
// transitions to a terminal state.
type LogFlusher interface {
	// FlushLogs drains all remaining buffered lines for the given
	// (taskID, attemptID) pair and blocks until the drain is complete.
	// It is idempotent: calling it multiple times is safe.
	FlushLogs(ctx context.Context, taskID, attemptID string) error
}

// TaskLifecycleHook is an optional interface that an [OutputHandler] may
// implement to participate in the per-task execution lifecycle.
//
// The executor calls [RegisterTask] before starting each task process and
// [Deregister] after the task completes, giving the handler a per-task cancel
// function it can call when an OpenJD fail directive is seen.  After the process
// exits, the executor calls [TakeFailReason] to retrieve any stored failure
// reason before publishing the terminal status message.
//
// The OpenJD interceptor (internal/worker/openjd) implements this interface so
// that openjd_fail directives can terminate a specific task without canceling
// the worker-wide context.
type TaskLifecycleHook interface {
	// RegisterTask associates task metadata and a cancel function with
	// attemptID.  The cancel function is called when an openjd_fail directive
	// is seen; termination then honors the action's OpenJD cancelation method
	// (TERMINATE by default → immediate SIGKILL; NOTIFY_THEN_TERMINATE →
	// SIGTERM then SIGKILL after the notify period).
	RegisterTask(attemptID, taskID, jobID, sessionID string, cancel context.CancelFunc)

	// Deregister removes the per-task state for attemptID.  Safe to call
	// multiple times.
	Deregister(attemptID string)

	// TakeFailReason returns the failure reason stored by an openjd_fail
	// directive for attemptID and true if one is present.  Returns ("", false)
	// if no openjd_fail was seen.
	TakeFailReason(attemptID string) (string, bool)
}

// DiscardOutput is an [OutputHandler] that silently drops every line.
// Use it in tests or when output logging is intentionally disabled.
type DiscardOutput struct{}

// HandleLine discards the output line.
func (DiscardOutput) HandleLine(_ context.Context, _, _, _, _, _ string) {}

// LogOutput is an [OutputHandler] that forwards each output line to the
// structured logger at debug level.  This is the default handler used when
// nil is passed to [New].
type LogOutput struct {
	Logger *slog.Logger
}

// HandleLine logs the output line at debug level.
func (l LogOutput) HandleLine(ctx context.Context, taskID, attemptID, sessionID, stream, line string) {
	l.Logger.DebugContext(
		ctx, "executor: task output",
		slog.String("task_id", taskID),
		slog.String("attempt_id", attemptID),
		slog.String("session_id", sessionID),
		slog.String("stream", stream),
		slog.String("line", line),
	)
}

// ── Config ────────────────────────────────────────────────────────────────────

// Config holds the tunable parameters for an [Executor].
type Config struct {
	// KillGracePeriod is the time the executor waits after sending SIGTERM
	// before escalating to SIGKILL when forcibly terminating a process during
	// worker force-shutdown (DrainAndShutdown). Defaults to 10 s when zero or
	// negative.
	//
	// Note: per-task timeout and per-task cancellation no longer use this value;
	// they follow the assignment's OpenJD Action.cancelation method (TERMINATE =
	// immediate SIGKILL; NOTIFY_THEN_TERMINATE = SIGTERM then the action's notify
	// period). KillGracePeriod governs only the worker-shutdown grace window.
	KillGracePeriod time.Duration

	// AllowRoot, when true, allows the worker to run as the root user on
	// Linux/macOS.  [CheckRootUser] returns an error if this is false and the
	// process UID is 0.
	AllowRoot bool
}

// ── CancelRegistrar ───────────────────────────────────────────────────────────

// CancelRegistrar is an optional hook that the executor calls to subscribe to
// and unsubscribe from per-task cancel NATS messages.
//
// Register is called after the per-task context is set up, immediately before
// the task process starts.  Deregister is called when the task goroutine exits.
//
// Implementations must be safe for concurrent use from multiple goroutines
// (multiple tasks may register/deregister simultaneously).
type CancelRegistrar interface {
	// Register subscribes to cancel messages for the given taskID.
	// Errors are logged as warnings and do not abort task execution.
	Register(taskID string) error
	// Deregister unsubscribes from cancel messages for the given taskID.
	Deregister(taskID string)
}

// ── Internal types ────────────────────────────────────────────────────────────

// taskRun holds the in-memory runtime state of an executing task.  It is
// stored in [Executor.activeTasks] from Dispatch until the task goroutine
// exits.
type taskRun struct {
	taskID    string
	attemptID string
	sessionID string
	jobID     string

	// pid is set after the OS process has been started successfully.
	pid int
	// startedAt is the wall-clock time of os.Process.Start().
	startedAt time.Time

	// cancelFunc is the per-task context cancellation function, set by
	// runTask after creating taskCtx.  External Cancel() calls use it to
	// interrupt the running process via SIGTERM → SIGKILL escalation.
	// Protected by Executor.mu; nil until runTask sets it.
	cancelFunc context.CancelFunc

	// cancelRequested is true when Cancel() was called before cancelFunc was
	// set (i.e., the task goroutine hasn't initialized its context yet).
	// Protected by Executor.mu; checked by runTask on startup.
	cancelRequested bool
}

// ── Executor ─────────────────────────────────────────────────────────────────

// Executor starts and manages bare-metal OS processes for assigned tasks.
//
// Create an instance with [New] and wire it into the pull loop and heartbeat
// publisher.  Call [CheckRootUser] before creating an Executor to verify the
// process is not running as root.  After construction, call
// [SetCancelRegistrar] to wire in the NATS cancel subscription handler.
type Executor struct {
	statusPub     *status.Publisher
	sessionMgr    *session.Manager
	m             *metrics.Metrics
	outputHandler OutputHandler
	logger        *slog.Logger
	cfg           Config

	mu               sync.Mutex
	activeTasks      map[string]*taskRun
	lastAssignmentAt *time.Time

	// cancelReg, if non-nil, is notified when tasks start and end so it can
	// subscribe to per-task cancel NATS messages.
	// Protected by mu; set via SetCancelRegistrar before any Dispatch calls.
	cancelReg CancelRegistrar

	// execCtx / execCancel control the lifetime of all task goroutines (tasks
	// 86–87).  Task goroutines derive their taskCtx from execCtx rather than
	// from the worker's signal context, so they survive a SIGINT/SIGTERM and
	// are only killed when DrainAndShutdown explicitly cancels execCtx after
	// the shutdown grace period expires.
	execCtx    context.Context
	execCancel context.CancelFunc

	// shuttingDown is set to true immediately before execCancel() is called in
	// DrainAndShutdown.  runTask checks this flag in the result.Canceled branch
	// to distinguish a force-shutdown kill (publish "failed"/"worker_shutdown")
	// from a normal per-task server cancel (publish "canceled").
	shuttingDown atomic.Bool

	// wg tracks active task goroutines so DrainAndShutdown can block until all
	// have published their terminal statuses and exited.
	wg sync.WaitGroup
}

// New creates a ready-to-use Executor.
//
// statusPub is the typed status publisher used for all task-status messages;
// it must not be nil.
//
// outputHandler may be nil; if so [LogOutput] is used to forward process
// output to the structured logger.
//
// cfg.KillGracePeriod defaults to 10 s if zero or negative.
func New(
	statusPub *status.Publisher,
	sessionMgr *session.Manager,
	m *metrics.Metrics,
	outputHandler OutputHandler,
	cfg Config,
	logger *slog.Logger,
) *Executor {
	if cfg.KillGracePeriod <= 0 {
		cfg.KillGracePeriod = 10 * time.Second
	}
	if outputHandler == nil {
		outputHandler = LogOutput{Logger: logger}
	}

	execCtx, execCancel := context.WithCancel(context.Background())

	return &Executor{
		statusPub:     statusPub,
		sessionMgr:    sessionMgr,
		m:             m,
		outputHandler: outputHandler,
		logger:        logger,
		cfg:           cfg,
		activeTasks:   make(map[string]*taskRun),
		execCtx:       execCtx,
		execCancel:    execCancel,
	}
}

// ── pull.TaskDispatcher ───────────────────────────────────────────────────────

// Dispatch implements [pull.TaskDispatcher].
//
// It creates a session and launches a goroutine that executes the task
// process.  Dispatch itself returns quickly; the task goroutine runs
// independently.  The server gates concurrency by CPU cores; the worker
// runs whatever it is leased.
func (e *Executor) Dispatch(ctx context.Context, msg *protocol.AssignMsg) error {
	// Create the session.
	sess, err := e.sessionMgr.Create(ctx, msg)
	if err != nil {
		return fmt.Errorf("executor: create session: %w", err)
	}

	run := &taskRun{
		taskID:    msg.TaskID,
		attemptID: msg.AttemptID,
		sessionID: sess.ID,
		jobID:     msg.JobID,
	}

	// wg.Add(1) before addActiveTask closes the TOCTOU window: if DrainAndShutdown
	// polls activeTasks and sees this task, the WaitGroup counter is already ≥1,
	// so wg.Wait() will not return prematurely.
	e.wg.Add(1)
	e.addActiveTask(run)

	// Launch the task goroutine.  ctx is the worker's signal context used only
	// for log call context; task execution is gated on e.execCtx, which
	// DrainAndShutdown cancels after the shutdown grace period.
	go e.runTask(ctx, msg, sess, run) //nolint:gosec // G118: ctx used only for logging; e.execCtx governs task lifetime

	return nil
}

// ── pull.StateSource / heartbeat.StateSource ──────────────────────────────────

// ActiveTaskCount returns the number of task processes currently executing.
// Implements both [pull.StateSource] and [heartbeat.StateSource].
func (e *Executor) ActiveTaskCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.activeTasks)
}

// ActiveTaskIDs returns a snapshot of the IDs of all currently running tasks.
// Implements [heartbeat.StateSource].
func (e *Executor) ActiveTaskIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.activeTasks))
	for id := range e.activeTasks {
		ids = append(ids, id)
	}
	return ids
}

// LastAssignmentAt returns the time of the most recent task assignment
// dispatched to this executor, or nil if no task has been dispatched yet.
// Implements [heartbeat.StateSource].
func (e *Executor) LastAssignmentAt() *time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastAssignmentAt
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// addActiveTask registers run in the active-tasks map, updates the
// last-assignment timestamp, and increments the Prometheus gauge.
func (e *Executor) addActiveTask(run *taskRun) {
	now := time.Now()
	e.mu.Lock()
	e.activeTasks[run.taskID] = run
	e.lastAssignmentAt = &now
	e.mu.Unlock()
	e.m.ActiveTasks.Inc()
}

// removeActiveTask removes taskID from the active-tasks map and decrements
// the Prometheus gauge.
func (e *Executor) removeActiveTask(taskID string) {
	e.mu.Lock()
	delete(e.activeTasks, taskID)
	e.mu.Unlock()
	e.m.ActiveTasks.Dec()
}

// applyCancelFunc stores taskCancel in run (under e.mu) and calls it
// immediately if Cancel() was called before runTask set up the context
// (cancelRequested flag path).
func (e *Executor) applyCancelFunc(run *taskRun, taskCancel context.CancelFunc) {
	e.mu.Lock()
	run.cancelFunc = taskCancel
	early := run.cancelRequested
	e.mu.Unlock()
	if early {
		taskCancel()
	}
}

// registerCancelSubscription subscribes to the per-task NATS cancel subject
// via the configured CancelRegistrar. It returns a cleanup
// function that the caller must defer — it calls Deregister when the task
// goroutine exits.
//
// If no CancelRegistrar is wired, the returned function is a no-op.
func (e *Executor) registerCancelSubscription(ctx context.Context, taskID string) func() {
	e.mu.Lock()
	cr := e.cancelReg
	e.mu.Unlock()
	if cr == nil {
		return func() {}
	}
	if regErr := cr.Register(taskID); regErr != nil {
		e.logger.WarnContext(
			ctx, "executor: cancel subscription registration failed",
			slog.String("task_id", taskID),
			slog.Any("error", regErr),
		)
		// Nothing was registered so there is nothing to deregister.
		return func() {}
	}
	return func() { cr.Deregister(taskID) }
}

// lastProgress returns the most-recently seen openjd_progress value for
// attemptID, or nil if the output handler does not implement progress
// tracking or no progress directive has been seen yet.
//
// It uses an anonymous interface check so the executor package does not need
// to import openjd.
func (e *Executor) lastProgress(attemptID string) *int {
	type progressReader interface {
		LastProgress(string) *int
	}
	if pr, ok := e.outputHandler.(progressReader); ok {
		return pr.LastProgress(attemptID)
	}
	return nil
}

// SetCancelRegistrar wires a [CancelRegistrar] into the executor so that
// per-task NATS cancel subscriptions are managed automatically.
//
// Must be called before any task is dispatched; it is safe to call
// concurrently with other methods but races with active Dispatch calls are
// not supported.
func (e *Executor) SetCancelRegistrar(cr CancelRegistrar) {
	e.mu.Lock()
	e.cancelReg = cr
	e.mu.Unlock()
}

// Cancel requests cancellation of the task identified by taskID.
//
// If the task is actively executing its per-task context is canceled,
// triggering termination per the assignment's OpenJD Action.cancelation method
// (TERMINATE by default → immediate SIGKILL; NOTIFY_THEN_TERMINATE → SIGTERM
// then SIGKILL after the notify period).
// If the task is known but its process has not yet started (race between
// Dispatch and goroutine scheduling), the cancel is noted and takes effect
// as soon as runTask initializes its context.
//
// Returns true if taskID was found in the active-tasks map (regardless of
// whether cancellation completed), false if the task was not found (already
// finished or never dispatched).
func (e *Executor) Cancel(taskID string) bool {
	e.mu.Lock()
	run, ok := e.activeTasks[taskID]
	if !ok {
		e.mu.Unlock()
		return false
	}
	if run.cancelFunc != nil {
		f := run.cancelFunc
		e.mu.Unlock()
		f()
		return true
	}
	// cancelFunc not yet set: the runTask goroutine hasn't initialized its
	// per-task context yet.  Flag the run so runTask handles it on startup.
	run.cancelRequested = true
	e.mu.Unlock()
	return true
}

// DrainAndShutdown implements the worker graceful-shutdown sequence (tasks
// 86–87).
//
// It first waits up to gracePeriod for all in-flight tasks to complete
// naturally.  If tasks are still running when the grace period expires, it
// sets the shuttingDown flag (so goroutines publish "failed"/"worker_shutdown"
// rather than "canceled") and cancels the shared execCtx, triggering
// SIGTERM → SIGKILL escalation in each runTask goroutine.  It then blocks
// until all goroutines have published their terminal statuses and exited.
//
// Returns (completed, killed): the number of tasks that finished naturally
// during the grace period vs the number that were force-terminated.
//
// A gracePeriod of zero skips the wait phase and force-kills immediately.
func (e *Executor) DrainAndShutdown(gracePeriod time.Duration) (completed, killed int) {
	e.mu.Lock()
	initial := len(e.activeTasks)
	e.mu.Unlock()

	if initial == 0 {
		return 0, 0
	}

	if gracePeriod > 0 {
		// Poll until all tasks complete naturally or the grace period expires.
		timer := time.NewTimer(gracePeriod)
		defer timer.Stop()
		poll := time.NewTicker(50 * time.Millisecond)
		defer poll.Stop()

	drainLoop:
		for {
			select {
			case <-timer.C:
				break drainLoop
			case <-poll.C:
				e.mu.Lock()
				remaining := len(e.activeTasks)
				e.mu.Unlock()
				if remaining == 0 {
					// All tasks completed naturally within the grace period.
					e.wg.Wait()
					return initial, 0
				}
			}
		}
	}

	// Grace period expired (or gracePeriod == 0): snapshot the survivors, set
	// the shutdown flag, and cancel all task execution contexts.
	e.mu.Lock()
	killedIDs := make([]string, 0, len(e.activeTasks))
	for taskID := range e.activeTasks {
		killedIDs = append(killedIDs, taskID)
	}
	e.mu.Unlock()

	killed = len(killedIDs)
	completed = initial - killed

	e.shuttingDown.Store(true)
	e.execCancel() // cancels all task execution contexts → SIGTERM → SIGKILL

	// Block until every goroutine has published its terminal status and exited.
	e.wg.Wait()
	return
}

// FlushShutdownStatuses publishes a "failed"/"worker_shutdown" status for
// every task currently in the active-tasks map.
//
// This must be called before draining the NATS connection during worker
// shutdown so the server learns about task failures immediately rather than
// waiting for the heartbeat-timeout sweep.  Each task goroutine will also
// publish its own terminal status as it exits; the server must handle
// duplicate terminal statuses gracefully (accept the first, ignore subsequent).
//
// The total flush is bounded by a 5-second deadline so that a saturated NATS
// connection does not stall the shutdown sequence indefinitely.
func (e *Executor) FlushShutdownStatuses() {
	e.mu.Lock()
	tasks := make([]status.ShutdownTask, 0, len(e.activeTasks))
	for _, run := range e.activeTasks {
		tasks = append(tasks, status.ShutdownTask{
			TaskID:    run.taskID,
			AttemptID: run.attemptID,
			JobID:     run.jobID,
			SessionID: run.sessionID,
		})
	}
	e.mu.Unlock()

	if len(tasks) == 0 {
		return
	}
	e.logger.InfoContext(
		context.Background(),
		"executor: publishing worker_shutdown status for in-flight tasks",
		slog.Int("count", len(tasks)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.statusPub.ShutdownFailed(ctx, tasks)
}
