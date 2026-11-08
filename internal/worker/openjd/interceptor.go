// SPDX-License-Identifier: AGPL-3.0-or-later

// Package openjd implements the OpenJD progress-line interceptor for sqi-worker.
//
// An [Interceptor] wraps a downstream output handler and inspects each process
// stdout line for recognized OpenJD directive prefixes:
//
// - openjd_progress: <0-100> — updates the task's progress percentage (any
// finite number, stored as an integer percent rounded down)
// - openjd_status: <text> — updates the task's status text
// - openjd_fail: <text> — cancels the per-task context and stores a failure reason
//
// Changes to progress or status are published as a "running" snapshot via NATS,
// throttled to one per [DefaultProgressInterval] with a trailing send.
//
// All other lines — including unrecognized openjd_* prefixes and all stderr
// lines — are forwarded to the downstream handler unmodified.
//
// # SQI_LOG_PROGRESS
//
// When the executor installs a [logprogress.Matcher] for an attempt with
// [Interceptor.SetLogProgress], every forwarded line, stdout and stderr, is also
// fed to it, except an openjd_redacted_env directive line. A match updates the
// same snapshot the directives do, under the same throttle. Per field, a
// directive wins: once the attempt has printed a valid openjd_progress line the
// patterns no longer set progress, and once it has printed an openjd_status line
// they no longer set status.
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
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/openjd/logprogress"
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

// DefaultProgressInterval is the minimum spacing between published
// progress/status snapshots for one attempt.
const DefaultProgressInterval = 2 * time.Second

// ── Directive constants ───────────────────────────────────────────────────────

// Recognized OpenJD directive prefixes.
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
	progress   *int   // nil until the first valid openjd_progress line
	failReason string // set on the first openjd_fail line
	failed     bool   // true once openjd_fail fires; prevents duplicate handling
	statusText string // latest openjd_status text

	lastSent time.Time   // when the last snapshot was published
	pending  bool        // a change is waiting for the trailing send
	timer    *time.Timer // trailing-send timer, armed while pending
	finished bool        // no further snapshots may be sent

	// matcher is the attempt's SQI_LOG_PROGRESS matcher, nil when the step
	// declares none. directiveProgress and directiveStatus record that the
	// attempt printed an openjd_progress or openjd_status line, after which the
	// matcher no longer sets that field.
	matcher           *logprogress.Matcher
	directiveProgress bool
	directiveStatus   bool
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
	interval   time.Duration

	mu       sync.Mutex
	attempts map[string]*attemptState // keyed by attemptID
}

// New returns a ready-to-use Interceptor.
//
// downstream receives all non-directive stdout lines and all stderr lines.
// nc is used to publish openjd_status messages to the task.status NATS subject.
// workerID is embedded in every status message published by the interceptor.
func New(
	downstream outputHandler, nc natsPublisher, workerID string, logger *slog.Logger, opts ...Option,
) *Interceptor {
	i := &Interceptor{
		downstream: downstream,
		nc:         nc,
		workerID:   workerID,
		logger:     logger,
		interval:   DefaultProgressInterval,
		attempts:   make(map[string]*attemptState),
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

// Option configures an [Interceptor].
type Option func(*Interceptor)

// WithProgressInterval sets the minimum spacing between published snapshots.
func WithProgressInterval(d time.Duration) Option {
	return func(i *Interceptor) { i.interval = d }
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

// SetLogProgress installs m as attemptID's SQI_LOG_PROGRESS matcher. It must be
// called after [Interceptor.RegisterTask] and before the attempt's process
// starts. A nil m, or an attemptID that is not registered, is a no-op.
func (i *Interceptor) SetLogProgress(attemptID string, m *logprogress.Matcher) {
	if m == nil {
		return
	}
	i.mu.Lock()
	st := i.attempts[attemptID]
	i.mu.Unlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	st.matcher = m
	st.mu.Unlock()
}

// Deregister removes the per-task state for attemptID and cancels any pending
// snapshot send.  Safe to call multiple times.
func (i *Interceptor) Deregister(attemptID string) {
	i.mu.Lock()
	st := i.attempts[attemptID]
	delete(i.attempts, attemptID)
	i.mu.Unlock()
	if st != nil {
		st.finish()
	}
}

// finish stops the trailing send and refuses all later sends.
func (st *attemptState) finish() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.finishLocked()
}

// finishLocked is [attemptState.finish] with st.mu already held.
func (st *attemptState) finishLocked() {
	st.finished = true
	st.pending = false
	if st.timer != nil {
		st.timer.Stop()
	}
}

// FinishProgress refuses all later sends for attemptID and returns the last
// progress, as [Interceptor.LastProgress] does. A change still waiting for its
// trailing send is published before it returns, so the final progress and
// status reach the server. The executor calls it before publishing the
// terminal status, so that snapshot precedes the terminal message and no late
// snapshot can follow it.
func (i *Interceptor) FinishProgress(attemptID string) *int {
	i.mu.Lock()
	st := i.attempts[attemptID]
	i.mu.Unlock()
	if st == nil {
		return nil
	}
	st.mu.Lock()
	flush := st.pending && !st.finished
	var msg protocol.TaskStatusMsg
	if flush {
		msg = i.snapshotLocked(attemptID, st, time.Now())
	}
	st.finishLocked()
	st.mu.Unlock()
	if flush {
		i.publish(context.Background(), attemptID, msg)
	}
	return i.LastProgress(attemptID)
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
	// The failed flag, not an empty failReason, distinguishes
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
// that include the last-known progress in status messages.
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
// forwards everything else to the downstream handler unmodified.
//
// Stderr lines are always forwarded without directive inspection because
// tasks are expected to emit directives only to stdout.  Recognized directive
// lines (openjd_progress, openjd_status, openjd_fail) are consumed without
// forwarding; all other lines — including unrecognized openjd_* prefixes —
// are passed through, and then fed to the attempt's SQI_LOG_PROGRESS matcher,
// if any.
func (i *Interceptor) HandleLine(ctx context.Context, taskID, attemptID, sessionID, stream, line string) {
	// stderr is never intercepted; forward immediately.
	if stream == "stderr" {
		i.downstream.HandleLine(ctx, taskID, attemptID, sessionID, stream, line)
		i.matchProgress(ctx, attemptID, line)
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
		// Unrecognized openjd_* prefixes and normal lines pass through.
		i.downstream.HandleLine(ctx, taskID, attemptID, sessionID, stream, line)
		i.matchProgress(ctx, attemptID, line)
	}
}

// matchProgress feeds a forwarded line to attemptID's SQI_LOG_PROGRESS matcher
// and applies what it reports, except a field a directive has already set.
// An openjd_redacted_env directive line carries its secret in the clear, so it
// is never matched.
func (i *Interceptor) matchProgress(ctx context.Context, attemptID, line string) {
	i.mu.Lock()
	st := i.attempts[attemptID]
	i.mu.Unlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	m := st.matcher
	st.mu.Unlock()
	if m == nil || IsRedactedEnvDirective(line) {
		return
	}
	u := m.Observe(line)
	if !u.HasProgress && !u.HasStatus {
		return
	}
	var text string
	if u.HasStatus {
		text = protocol.SanitizeStatusText(u.Status)
	}

	if st.applyPattern(u, text) {
		i.noteChange(ctx, attemptID, st)
	}
}

// applyPattern records a matcher update, with status already sanitized as
// text, skipping a field a directive has set. It reports whether anything
// changed.
func (st *attemptState) applyPattern(u logprogress.Update, text string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	changed := false
	if u.HasProgress && !st.directiveProgress && (st.progress == nil || *st.progress != u.Progress) {
		v := u.Progress
		st.progress = &v
		changed = true
	}
	if u.HasStatus && text != "" && !st.directiveStatus && st.statusText != text {
		st.statusText = text
		changed = true
	}
	return changed
}

// ── directive handlers ────────────────────────────────────────────────────────

// parseProgress parses an openjd_progress value: any finite number from 0 to
// 100, returned as an integer percent rounded down.
func parseProgress(text string) (int, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
		return 0, false
	}
	return int(math.Floor(v)), true
}

// handleProgress parses an openjd_progress directive and records it.
//
// Format: "openjd_progress: <number 0–100>"
//
// Lines with the recognized prefix but an invalid value are logged as warnings
// and not forwarded.
func (i *Interceptor) handleProgress(ctx context.Context, attemptID, line string) {
	text := strings.TrimSpace(strings.TrimPrefix(line, prefixProgress))
	val, ok := parseProgress(text)
	if !ok {
		i.logger.WarnContext(
			ctx, "openjd: invalid progress value — ignoring",
			slog.String("attempt_id", attemptID),
			slog.String("raw", text),
		)
		return
	}

	i.mu.Lock()
	st, found := i.attempts[attemptID]
	i.mu.Unlock()
	if !found {
		return
	}

	st.mu.Lock()
	changed := st.progress == nil || *st.progress != val
	st.progress = &val
	st.directiveProgress = true
	st.mu.Unlock()

	i.logger.DebugContext(
		ctx, "openjd: progress update",
		slog.String("attempt_id", attemptID),
		slog.Int("progress", val),
	)
	if changed {
		i.noteChange(ctx, attemptID, st)
	}
}

// handleStatus parses an openjd_status directive and records it.
//
// Format: "openjd_status: <text>".
func (i *Interceptor) handleStatus(ctx context.Context, attemptID, line string) {
	text := protocol.SanitizeStatusText(strings.TrimPrefix(line, prefixStatus))

	i.mu.Lock()
	st, ok := i.attempts[attemptID]
	i.mu.Unlock()
	if !ok {
		return
	}

	st.mu.Lock()
	changed := st.statusText != text
	st.statusText = text
	st.directiveStatus = true
	st.mu.Unlock()

	i.logger.DebugContext(
		ctx, "openjd: status update",
		slog.String("attempt_id", attemptID),
		slog.String("text", text),
	)
	if changed {
		i.noteChange(ctx, attemptID, st)
	}
}

// noteChange publishes a snapshot now when the throttle window has elapsed,
// otherwise arms a single trailing send for the end of the window.
func (i *Interceptor) noteChange(ctx context.Context, attemptID string, st *attemptState) {
	st.mu.Lock()
	if st.finished {
		st.mu.Unlock()
		return
	}
	now := time.Now()
	if elapsed := now.Sub(st.lastSent); st.lastSent.IsZero() || elapsed >= i.interval {
		i.sendLockedThenPublish(ctx, attemptID, st, now)
		return
	}
	st.pending = true
	if st.timer == nil {
		st.timer = time.AfterFunc(st.lastSent.Add(i.interval).Sub(now), func() {
			i.sendTrailing(attemptID, st)
		})
	} else {
		st.timer.Reset(st.lastSent.Add(i.interval).Sub(now))
	}
	st.mu.Unlock()
}

// sendTrailing publishes the pending snapshot, if one is still wanted.
func (i *Interceptor) sendTrailing(attemptID string, st *attemptState) {
	st.mu.Lock()
	if !st.pending || st.finished {
		st.mu.Unlock()
		return
	}
	i.sendLockedThenPublish(context.Background(), attemptID, st, time.Now())
}

// sendLockedThenPublish records a send at now, then publishes the snapshot. It
// is called with st.mu held and returns with it released: the publish itself
// runs outside the lock.
func (i *Interceptor) sendLockedThenPublish(ctx context.Context, attemptID string, st *attemptState, now time.Time) {
	st.lastSent = now
	st.pending = false
	msg := i.snapshotLocked(attemptID, st, now)
	st.mu.Unlock()
	i.publish(ctx, attemptID, msg)
}

// snapshotLocked builds the snapshot message; st.mu must be held.
func (i *Interceptor) snapshotLocked(attemptID string, st *attemptState, now time.Time) protocol.TaskStatusMsg {
	var lastProgress *int
	if st.progress != nil {
		v := *st.progress
		lastProgress = &v
	}
	return protocol.TaskStatusMsg{
		Version:      protocol.ProtocolVersion,
		Type:         protocol.TypeTaskStatus,
		TaskID:       st.taskID,
		AttemptID:    attemptID,
		JobID:        st.jobID,
		Status:       "running",
		SessionID:    st.sessionID,
		WorkerID:     i.workerID,
		LastProgress: lastProgress,
		At:           now,
		Message:      st.statusText,
	}
}

// publish sends a snapshot.  Snapshots are best-effort live UI updates, not
// terminal state transitions, so a failure is logged and dropped rather than
// retried (unlike the status.Publisher path).
func (i *Interceptor) publish(ctx context.Context, attemptID string, msg protocol.TaskStatusMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		i.logger.WarnContext(
			ctx, "openjd: marshal status update failed",
			slog.String("attempt_id", attemptID),
			slog.Any("error", err),
		)
		return
	}
	if err := i.nc.Publish(bus.TaskStatusSubject(i.workerID, msg.JobID), data); err != nil {
		i.logger.WarnContext(
			ctx, "openjd: publish status update failed",
			slog.String("attempt_id", attemptID),
			slog.Any("error", err),
		)
	}
}

// handleFail parses an openjd_fail directive, stores the failure reason, and
// calls the per-task cancel function to begin SIGTERM → SIGKILL escalation.
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
