// SPDX-License-Identifier: AGPL-3.0-only

// Package executor implements the bare-metal task executor for sqi-worker.
//
// An [Executor] starts OS processes via [os/exec] for each assigned task,
// captures their stdout and stderr line-by-line, manages per-task execution
// timeouts (SIGTERM → SIGKILL after a configurable grace period), and publishes
// task-status messages back to sqi-server via NATS.
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
// A buffered channel semaphore of size [Config.MaxConcurrentTasks] is the
// authoritative concurrency gate.  The pull loop already checks available
// slots before calling Dispatch; the semaphore provides a second, race-safe
// enforcement layer for cases where multiple queue consumer goroutines call
// Dispatch concurrently.
//
// # Output handling
//
// The [OutputHandler] interface abstracts what happens with each line of
// process output.  The log-chunk publisher (tasks 64–69) and OpenJD progress
// line parser (tasks 70–74) will implement this interface; until then the
// [LogOutput] implementation forwards lines to the structured logger.
//
// # Root-user check
//
// [CheckRootUser] should be called before creating an Executor.  It returns
// an error if the worker process is running as root on Linux/macOS and
// [Config.AllowRoot] is false (task 57, sqi.md §18, open question 2).
package executor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// ── Interfaces ────────────────────────────────────────────────────────────────

// OutputHandler processes lines of output emitted by a running task process.
// Implementations include the log-chunk publisher (tasks 64–69) and the
// OpenJD progress-line parser (tasks 70–74).  An OutputHandler must be safe
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
// the terminal status message is published (task 68).
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
	// is seen, triggering per-task SIGTERM → SIGKILL escalation.
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
	// MaxConcurrentTasks is the maximum number of task processes that may
	// execute simultaneously.  Must be ≥ 1; enforced via a buffered channel
	// semaphore.
	MaxConcurrentTasks int

	// KillGracePeriod is the time the executor waits after sending SIGTERM
	// before escalating to SIGKILL when forcibly terminating a process due to
	// a per-task timeout or worker shutdown.  Defaults to 10 s when zero or
	// negative.
	KillGracePeriod time.Duration

	// AllowRoot, when true, allows the worker to run as the root user on
	// Linux/macOS.  [CheckRootUser] returns an error if this is false and the
	// process UID is 0 (task 57).
	AllowRoot bool
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
	// startedAt is the wall-clock time of os.Process.Start() (task 53).
	startedAt time.Time
}

// ── Executor ─────────────────────────────────────────────────────────────────

// Executor starts and manages bare-metal OS processes for assigned tasks.
//
// Create an instance with [New] and wire it into the pull loop and heartbeat
// publisher.  Call [CheckRootUser] before creating an Executor to verify the
// process is not running as root.
type Executor struct {
	statusPub     *status.Publisher
	sessionMgr    *session.Manager
	m             *metrics.Metrics
	outputHandler OutputHandler
	logger        *slog.Logger
	cfg           Config

	// sem is a counting semaphore implemented as a buffered channel.
	// A goroutine must receive a token from sem before starting a task and
	// must return it when the task terminates.  Buffer size = MaxConcurrentTasks.
	sem chan struct{}

	mu               sync.Mutex
	activeTasks      map[string]*taskRun
	lastAssignmentAt *time.Time
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
// cfg.MaxConcurrentTasks is clamped to a minimum of 1.
func New(
	statusPub *status.Publisher,
	sessionMgr *session.Manager,
	m *metrics.Metrics,
	outputHandler OutputHandler,
	cfg Config,
	logger *slog.Logger,
) *Executor {
	if cfg.MaxConcurrentTasks < 1 {
		cfg.MaxConcurrentTasks = 1
	}
	if cfg.KillGracePeriod <= 0 {
		cfg.KillGracePeriod = 10 * time.Second
	}
	if outputHandler == nil {
		outputHandler = LogOutput{Logger: logger}
	}

	// Pre-fill the semaphore so the first MaxConcurrentTasks acquires succeed
	// without blocking.
	sem := make(chan struct{}, cfg.MaxConcurrentTasks)
	for range cfg.MaxConcurrentTasks {
		sem <- struct{}{}
	}

	return &Executor{
		statusPub:     statusPub,
		sessionMgr:    sessionMgr,
		m:             m,
		outputHandler: outputHandler,
		logger:        logger,
		cfg:           cfg,
		sem:           sem,
		activeTasks:   make(map[string]*taskRun),
	}
}

// ── pull.TaskDispatcher ───────────────────────────────────────────────────────

// Dispatch implements [pull.TaskDispatcher].
//
// It acquires a concurrency slot (non-blocking — returns an error that causes
// the pull loop to nack the message if at capacity), creates a session, and
// launches a goroutine that executes the task process.  Dispatch itself
// returns quickly; the task goroutine runs independently.
func (e *Executor) Dispatch(ctx context.Context, msg *protocol.AssignMsg) error {
	// Task 56: acquire a concurrency slot.
	select {
	case <-e.sem:
		// Slot acquired.
	default:
		return fmt.Errorf("executor: at capacity (%d/%d tasks running)",
			e.ActiveTaskCount(), e.cfg.MaxConcurrentTasks)
	}

	// Create the session (tasks 43–45).  On failure, release the slot before
	// returning so the pull loop can nack and another worker can try.
	sess, err := e.sessionMgr.Create(ctx, msg)
	if err != nil {
		e.sem <- struct{}{} // release slot
		return fmt.Errorf("executor: create session: %w", err)
	}

	run := &taskRun{
		taskID:    msg.TaskID,
		attemptID: msg.AttemptID,
		sessionID: sess.ID,
		jobID:     msg.JobID,
	}

	e.addActiveTask(run)

	// Launch the task goroutine.  ctx propagates the worker shutdown signal;
	// the goroutine sends SIGTERM (then SIGKILL) to the child process and
	// publishes a terminal status before exiting.
	go e.runTask(ctx, msg, sess, run) //nolint:gosec // G118: ctx is request-scoped; context.Background() inside runTask is intentional for cleanup operations that must complete after shutdown

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

// removeActiveTask removes taskID from the active-tasks map, decrements the
// Prometheus gauge, and returns the concurrency slot to the semaphore so the
// pull loop can fetch another assignment.
func (e *Executor) removeActiveTask(taskID string) {
	e.mu.Lock()
	delete(e.activeTasks, taskID)
	e.mu.Unlock()
	e.m.ActiveTasks.Dec()
	e.sem <- struct{}{} // release slot (task 56)
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

// FlushShutdownStatuses publishes a "failed"/"worker_shutdown" status for
// every task currently in the active-tasks map (task 78).
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
