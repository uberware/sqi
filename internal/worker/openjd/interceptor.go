// SPDX-License-Identifier: AGPL-3.0-only

// Package openjd implements the OpenJD progress-line interceptor for sqi-worker
// (tasks 70–74).
//
// An [Interceptor] wraps a downstream output handler and inspects each process
// stdout line for recognized OpenJD directive prefixes:
//
//   - openjd_progress: <0-100>  — updates the task's in-memory progress percentage (task 70)
//   - openjd_status: <text>     — publishes a live "running" status update via NATS (task 71)
//   - openjd_fail: <text>       — cancels the per-task context and stores a failure reason (task 72)
//
// All other lines — including unrecognized openjd_* prefixes and all stderr
// lines — are forwarded to the downstream handler unmodified (task 73).
//
// # Integration with executor.Executor
//
// [Interceptor] implements [executor.TaskLifecycleHook], which the executor
// calls to register per-task metadata (task ID, job ID, session ID, and a
// context.CancelFunc) before the process starts and to retrieve any stored
// failure reason after the process exits.
//
// The executor creates a per-task derived context so that openjd_fail cancels
// only the affected task rather than the worker-wide context.
package openjd

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Interfaces ────────────────────────────────────────────────────────────────

// outputHandler is the subset of executor.OutputHandler the Interceptor needs
// for the downstream.  Defined locally so the openjd package does not import
// the executor package.
type outputHandler interface {
	HandleLine(ctx context.Context, taskID, attemptID, sessionID, stream, line string)
}

// natsPublisher is the subset of [*nats.Conn] the Interceptor uses to publish
// intermediate status messages.  Defined as an interface so tests can inject a
// stub without a live NATS server.
type natsPublisher interface {
	Publish(subj string, data []byte) error
}

// ── Directive constants ───────────────────────────────────────────────────────

// Recognized OpenJD directive prefixes (tasks 70–72).
const (
	prefixProgress = "openjd_progress:"
	prefixStatus   = "openjd_status:"
	prefixFail     = "openjd_fail:"
)

// ── attemptState ─────────────────────────────────────────────────────────────

// attemptState holds the per-task runtime state tracked by the Interceptor.
type attemptState struct {
	// taskID, jobID, sessionID, and cancel are written once by RegisterTask and
	// are read-only thereafter; they do not require mu protection.
	taskID    string
	jobID     string
	sessionID string
	cancel    context.CancelFunc

	// The fields below are mutated after construction and must be accessed
	// under mu.
	mu         sync.Mutex
	progress   *int   // nil until the first valid openjd_progress line (task 70)
	failReason string // set on the first openjd_fail line (task 72)
	failed     bool   // true once openjd_fail fires; prevents duplicate handling
}

// ── Interceptor ───────────────────────────────────────────────────────────────

// Interceptor is an output handler that intercepts recognized OpenJD directive
// lines from process stdout before forwarding the rest to the downstream handler.
//
// Interceptor also implements executor.TaskLifecycleHook so the executor can
// register per-task metadata (including a cancel function for openjd_fail) and
// retrieve any stored failure reason after the process exits.
//
// Interceptor is safe for concurrent use from multiple goroutines.
type Interceptor struct {
	downstream outputHandler
	nc         natsPublisher
	workerID   string
	logger     *slog.Logger

	mu       sync.Mutex
	attempts map[string]*attemptState // keyed by attemptID
}

// New returns a ready-to-use Interceptor.
//
// downstream receives all non-directive stdout lines and all stderr lines.
// nc is used to publish openjd_status messages to the task.status NATS subject.
// workerID is embedded in every status message published by the interceptor
// (task 76).
func New(downstream outputHandler, nc natsPublisher, workerID string, logger *slog.Logger) *Interceptor {
	return &Interceptor{
		downstream: downstream,
		nc:         nc,
		workerID:   workerID,
		logger:     logger,
		attempts:   make(map[string]*attemptState),
	}
}

// ── executor.TaskLifecycleHook ────────────────────────────────────────────────

// RegisterTask associates the given task metadata and cancel function with
// attemptID.  The cancel function is called when an openjd_fail directive is
// seen, so the executor can terminate the specific task process without
// canceling the worker context.
//
// RegisterTask must be called before the first HandleLine invocation for the
// given attemptID.
func (i *Interceptor) RegisterTask(attemptID, taskID, jobID, sessionID string, cancel context.CancelFunc) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.attempts[attemptID] = &attemptState{
		taskID:    taskID,
		jobID:     jobID,
		sessionID: sessionID,
		cancel:    cancel,
	}
}

// Deregister removes the per-task state for attemptID.  Safe to call multiple times.
func (i *Interceptor) Deregister(attemptID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.attempts, attemptID)
}

// TakeFailReason returns the failure reason stored by the first openjd_fail
// directive seen for attemptID, and true if one was present.  Returns
// ("", false) if no openjd_fail was seen.
//
// The stored reason is not cleared by this call; Deregister cleans it up.
// Repeated calls return a consistent value between RegisterTask and Deregister.
// An empty reason string with ok=true means openjd_fail was seen with no text.
func (i *Interceptor) TakeFailReason(attemptID string) (string, bool) {
	i.mu.Lock()
	st, ok := i.attempts[attemptID]
	i.mu.Unlock()
	if !ok {
		return "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	// Use the failed flag — not an empty failReason — to distinguish
	// "openjd_fail not seen" from "openjd_fail seen with no text".
	if !st.failed {
		return "", false
	}
	return st.failReason, true
}

// LastProgress returns the most recent progress percentage (0–100) seen via
// openjd_progress for the given attemptID, or nil if none has been seen.
//
// This is not part of executor.TaskLifecycleHook; it is provided for callers
// that include the last-known progress in status messages (task 76).
func (i *Interceptor) LastProgress(attemptID string) *int {
	i.mu.Lock()
	st, ok := i.attempts[attemptID]
	i.mu.Unlock()
	if !ok {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.progress == nil {
		return nil
	}
	v := *st.progress
	return &v
}

// FlushLogs forwards a flush request to the downstream handler if it implements
// the optional FlushLogs method.  This allows the executor to call FlushLogs on
// the Interceptor (which it holds as the configured OutputHandler) and have the
// call reach the logstreamer that wraps underneath.
//
// A local interface is used instead of importing executor.LogFlusher to avoid a
// circular dependency between the openjd and executor packages.
func (i *Interceptor) FlushLogs(ctx context.Context, taskID, attemptID string) error {
	type logFlusher interface {
		FlushLogs(ctx context.Context, taskID, attemptID string) error
	}
	if lf, ok := i.downstream.(logFlusher); ok {
		return lf.FlushLogs(ctx, taskID, attemptID)
	}
	return nil
}

// ── executor.OutputHandler ────────────────────────────────────────────────────

// HandleLine intercepts recognized OpenJD directive lines from stdout and
// forwards everything else to the downstream handler unmodified (task 73).
//
// Stderr lines are always forwarded without inspection because tasks are
// expected to emit directives only to stdout.  Recognized directive lines
// (openjd_progress, openjd_status, openjd_fail) are consumed without
// forwarding; all other lines — including unrecognized openjd_* prefixes —
// are passed through.
func (i *Interceptor) HandleLine(ctx context.Context, taskID, attemptID, sessionID, stream, line string) {
	// stderr is never intercepted; forward immediately (task 73).
	if stream == "stderr" {
		i.downstream.HandleLine(ctx, taskID, attemptID, sessionID, stream, line)
		return
	}

	// Inspect stdout for known OpenJD directives.
	switch {
	case strings.HasPrefix(line, prefixProgress):
		i.handleProgress(ctx, attemptID, line)
	case strings.HasPrefix(line, prefixStatus):
		i.handleStatus(ctx, attemptID, line)
	case strings.HasPrefix(line, prefixFail):
		i.handleFail(ctx, attemptID, line)
	default:
		// Unrecognized openjd_* prefixes and normal lines pass through (task 73).
		i.downstream.HandleLine(ctx, taskID, attemptID, sessionID, stream, line)
	}
}

// ── directive handlers ────────────────────────────────────────────────────────

// handleProgress parses an openjd_progress directive and updates the task's
// in-memory progress percentage (task 70).
//
// Format: "openjd_progress: <integer 0–100>"
//
// Lines with the recognized prefix but an invalid value (non-integer or outside
// 0–100) are logged as warnings and not forwarded.
func (i *Interceptor) handleProgress(ctx context.Context, attemptID, line string) {
	text := strings.TrimSpace(strings.TrimPrefix(line, prefixProgress))
	val, err := strconv.Atoi(text)
	if err != nil || val < 0 || val > 100 {
		i.logger.WarnContext(
			ctx, "openjd: invalid progress value — ignoring",
			slog.String("attempt_id", attemptID),
			slog.String("raw", text),
		)
		return
	}

	i.mu.Lock()
	st, ok := i.attempts[attemptID]
	i.mu.Unlock()
	if !ok {
		return
	}

	st.mu.Lock()
	st.progress = &val
	st.mu.Unlock()

	i.logger.DebugContext(
		ctx, "openjd: progress update",
		slog.String("attempt_id", attemptID),
		slog.Int("progress", val),
	)
}

// handleStatus parses an openjd_status directive and publishes an intermediate
// "running" status update to the task.status NATS subject (task 71).
//
// The published message includes worker_id (task 76) and the current
// last_progress value so the UI can display both the live status text and the
// progress bar simultaneously.
//
// Format: "openjd_status: <text>".
func (i *Interceptor) handleStatus(ctx context.Context, attemptID, line string) {
	text := strings.TrimSpace(strings.TrimPrefix(line, prefixStatus))

	i.mu.Lock()
	st, ok := i.attempts[attemptID]
	i.mu.Unlock()
	if !ok {
		return
	}

	// Snapshot progress under the per-attempt lock so we don't race with
	// handleProgress on the same attempt.
	st.mu.Lock()
	var lastProgress *int
	if st.progress != nil {
		v := *st.progress
		lastProgress = &v
	}
	st.mu.Unlock()

	msg := protocol.TaskStatusMsg{
		Version:      protocol.ProtocolVersion,
		Type:         protocol.TypeTaskStatus,
		TaskID:       st.taskID,
		AttemptID:    attemptID,
		JobID:        st.jobID,
		Status:       "running",
		SessionID:    st.sessionID,
		WorkerID:     i.workerID,
		LastProgress: lastProgress,
		At:           time.Now(),
		Message:      text,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		i.logger.WarnContext(
			ctx, "openjd: marshal status update failed",
			slog.String("attempt_id", attemptID),
			slog.Any("error", err),
		)
		return
	}
	// Intermediate openjd_status publishes are best-effort: they are live UI
	// updates, not terminal state transitions, so a single transient failure is
	// logged and dropped rather than retried (unlike the status.Publisher path).
	if err := i.nc.Publish(bus.TaskStatusSubject(st.jobID), data); err != nil {
		i.logger.WarnContext(
			ctx, "openjd: publish status update failed",
			slog.String("attempt_id", attemptID),
			slog.Any("error", err),
		)
	}

	i.logger.DebugContext(
		ctx, "openjd: status update",
		slog.String("attempt_id", attemptID),
		slog.String("text", text),
	)
}

// handleFail parses an openjd_fail directive, stores the failure reason, and
// calls the per-task cancel function to begin SIGTERM → SIGKILL escalation
// (task 72).
//
// Format: "openjd_fail: <text>"
//
// If multiple openjd_fail lines are emitted before the process exits, only the
// first triggers cancellation; subsequent lines are silently ignored.
func (i *Interceptor) handleFail(ctx context.Context, attemptID, line string) {
	text := strings.TrimSpace(strings.TrimPrefix(line, prefixFail))

	i.mu.Lock()
	st, ok := i.attempts[attemptID]
	i.mu.Unlock()
	if !ok {
		return
	}

	st.mu.Lock()
	if st.failed {
		// Already triggered; ignore subsequent openjd_fail lines.
		st.mu.Unlock()
		return
	}
	st.failReason = text
	st.failed = true
	cancel := st.cancel
	st.mu.Unlock()

	i.logger.WarnContext(
		ctx, "openjd: task self-reported failure — terminating process",
		slog.String("attempt_id", attemptID),
		slog.String("reason", text),
	)

	if cancel != nil {
		cancel()
	}
}
