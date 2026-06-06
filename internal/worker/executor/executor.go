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
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
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

// natsPublisher is the subset of [*nats.Conn] used by Executor for
// publishing status messages.  Defined as an interface so unit tests can
// inject a stub without requiring a live NATS server.
type natsPublisher interface {
	Publish(subj string, data []byte) error
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
	nc            natsPublisher
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
// outputHandler may be nil; if so [LogOutput] is used to forward process
// output to the structured logger.
//
// cfg.KillGracePeriod defaults to 10 s if zero or negative.
// cfg.MaxConcurrentTasks is clamped to a minimum of 1.
func New(
	nc natsPublisher,
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
		nc:            nc,
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

// publishStatus encodes msg as JSON and publishes it to task.status.<job>.
// Errors are logged as warnings; the caller proceeds regardless — status loss
// is preferable to blocking the task lifecycle.
func (e *Executor) publishStatus(ctx context.Context, msg protocol.TaskStatusMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		e.logger.WarnContext(
			ctx, "executor: marshal status message failed",
			slog.String("task_id", msg.TaskID),
			slog.String("status", msg.Status),
			slog.Any("error", err),
		)
		return
	}
	subj := bus.TaskStatusSubject(msg.JobID)
	if err := e.nc.Publish(subj, data); err != nil {
		e.logger.WarnContext(
			ctx, "executor: publish status message failed",
			slog.String("task_id", msg.TaskID),
			slog.String("status", msg.Status),
			slog.String("subject", subj),
			slog.Any("error", err),
		)
	}
}

// runningStatus returns a TaskStatusMsg with Status = "running".
func runningStatus(msg *protocol.AssignMsg, sessionID string, at time.Time) protocol.TaskStatusMsg {
	return protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    msg.TaskID,
		AttemptID: msg.AttemptID,
		JobID:     msg.JobID,
		Status:    "running",
		SessionID: sessionID,
		At:        at,
	}
}

// terminalStatus returns a TaskStatusMsg for a terminal state
// (succeeded / failed / canceled).  exitCode may be nil for "canceled" states
// where no meaningful exit code is available.
func terminalStatus(
	msg *protocol.AssignMsg,
	sessionID, status string,
	exitCode *int,
	failureReason string,
	at time.Time,
) protocol.TaskStatusMsg {
	return protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    msg.TaskID,
		AttemptID: msg.AttemptID,
		JobID:     msg.JobID,
		Status:    status,
		SessionID: sessionID,
		At:        at,
		Message:   failureReason,
		ExitCode:  exitCode,
	}
}
