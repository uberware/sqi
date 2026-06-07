// SPDX-License-Identifier: AGPL-3.0-only

package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
)

// ── processResult ─────────────────────────────────────────────────────────────

// processResult captures the complete outcome of an OS process execution
// (task 53: PID, start time, end time, exit code).
type processResult struct {
	// ExitCode is the process exit code.  -1 if the process was killed by a
	// signal and the OS did not produce a meaningful exit code.
	ExitCode int
	// PID is the OS process ID, recorded after a successful Start.
	PID int
	// StartedAt is the wall-clock time when cmd.Start() returned successfully.
	StartedAt time.Time
	// EndedAt is the wall-clock time when the process was confirmed to have
	// exited (cmd.Wait() returned).
	EndedAt time.Time
	// Err is a non-nil error only for unexpected failures (failed to start,
	// unexpected wait error).  A non-zero exit code is NOT reflected in Err;
	// it is captured in ExitCode.  Process termination due to a signal (when
	// TimedOut or Canceled is true) also does not set Err.
	Err error
	// TimedOut is true when the process was killed because the per-task
	// OnRun.TimeoutSeconds was exceeded (task 55).
	TimedOut bool
	// Canceled is true when the process was killed because the parent context
	// was canceled (typically worker shutdown).
	Canceled bool
}

// duration returns the wall-clock execution time of the process.
func (r processResult) duration() time.Duration {
	return r.EndedAt.Sub(r.StartedAt)
}

// ── runTask ───────────────────────────────────────────────────────────────────

// runTask is the goroutine that drives the full lifecycle of a single task:
//
//  1. Validate OnRun action.
//  2. Build env and start the OS process.
//  3. Publish "running" status (task 77 — included here as integral to the
//     executor).
//  4. Read stdout/stderr concurrently, forwarding to the OutputHandler.
//  5. Honor per-task timeout and context cancellation.
//  6. Publish terminal status (succeeded/failed/canceled).
//  7. Update metrics.
//  8. Remove from active-tasks and release the concurrency slot.
//  9. Clean up the session.
//
// runTask is launched by [Executor.Dispatch] and runs independently until the
// task terminates.
func (e *Executor) runTask(ctx context.Context, msg *protocol.AssignMsg, sess *session.Session, run *taskRun) {
	// failed controls the keepFailedSessions behavior in Cleanup.
	failed := false

	// LIFO: removeActiveTask runs first (releases the semaphore slot so the
	// pull loop can fetch another assignment while session cleanup is still in
	// progress), then Cleanup runs.
	defer func() { e.sessionMgr.Cleanup(context.Background(), sess, failed) }()
	defer e.removeActiveTask(run.taskID)

	// ── Nil-action guard ──────────────────────────────────────────────────────
	if msg.OnRun == nil {
		e.logger.WarnContext(
			ctx, "executor: assignment has no OnRun action — treating as succeeded",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
		)
		e.publishStatus(ctx, runningStatus(msg, sess.ID, time.Now()))
		zero := 0
		e.publishStatus(ctx, terminalStatus(msg, sess.ID, "succeeded", &zero, "", time.Now()))
		e.m.TasksTotal.WithLabelValues("succeeded").Inc()
		return
	}

	// ── Validate command ──────────────────────────────────────────────────────
	if msg.OnRun.Command == "" {
		e.logger.ErrorContext(
			ctx, "executor: OnRun.Command is empty — failing task",
			slog.String("task_id", msg.TaskID),
		)
		failed = true
		e.publishStatus(ctx, runningStatus(msg, sess.ID, time.Now()))
		minusOne := -1
		e.publishStatus(ctx, terminalStatus(msg, sess.ID, "failed", &minusOne,
			"task has an empty command", time.Now()))
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		return
	}

	// ── Validate and parse path map (task 59 / 62) ────────────────────────────
	// Parse validates that every PathMapRule has a non-empty DestinationPath.
	// If any named location is unresolvable, we abort immediately and publish
	// a failed status so the server does not wait for heartbeat timeout.
	//
	// Pre-execution failures below intentionally publish a "running" status
	// immediately before the terminal "failed", consistent with the nil-action
	// and empty-command branches above and with the execProcess convention
	// ("Publish running status before Start so the server records the attempt").
	// Every attempt must have a running→terminal transition for the server's
	// state machine; the running status is not a claim that a process exists.
	lookup, err := pathmap.Parse(msg.PathMap)
	if err != nil {
		e.logger.ErrorContext(
			ctx, "executor: unresolvable path map — failing task",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.Any("error", err),
		)
		failed = true
		e.publishStatus(ctx, runningStatus(msg, sess.ID, time.Now()))
		minusOne := -1
		e.publishStatus(ctx, terminalStatus(msg, sess.ID, "failed", &minusOne, err.Error(), time.Now()))
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		return
	}

	// ── Write OpenJD path mapping file (task 61) ──────────────────────────────
	// Written before execProcess so the launched process (and any future
	// environment setup or teardown actions) can read it from the session
	// working directory.  An empty PathMap produces no file (no-op).
	if writeErr := pathmap.WritePathMappingFile(sess.WorkDir, msg.PathMap); writeErr != nil {
		e.logger.ErrorContext(
			ctx, "executor: failed to write path_mapping.json — failing task",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("work_dir", sess.WorkDir),
			slog.Any("error", writeErr),
		)
		failed = true
		e.publishStatus(ctx, runningStatus(msg, sess.ID, time.Now()))
		minusOne := -1
		e.publishStatus(ctx, terminalStatus(msg, sess.ID, "failed", &minusOne, writeErr.Error(), time.Now()))
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		return
	}

	// ── Execute process ───────────────────────────────────────────────────────
	result := e.execProcess(ctx, msg, sess, run, lookup)

	// ── Publish terminal status and update metrics ────────────────────────────
	switch {
	case result.Err != nil && !result.TimedOut && !result.Canceled:
		// Process failed to start or produced an unexpected wait error.
		failed = true
		exitCode := result.ExitCode
		reason := fmt.Sprintf("process error: %v", result.Err)
		e.logger.ErrorContext(
			ctx, "executor: task process error",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sess.ID),
			slog.Any("error", result.Err),
		)
		e.publishStatus(ctx, terminalStatus(msg, sess.ID, "failed", &exitCode, reason, result.EndedAt))
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		e.m.ExecDuration.WithLabelValues("failed").Observe(result.duration().Seconds())

	case result.Canceled:
		// Worker shutdown canceled the task.
		failed = true
		e.logger.InfoContext(
			ctx, "executor: task canceled (worker shutdown)",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sess.ID),
			slog.Int("pid", result.PID),
			slog.Duration("duration", result.duration()),
		)
		// ctx may already be done; use background context for the final publish.
		e.publishStatus(context.Background(), terminalStatus(msg, sess.ID, "canceled", nil, "worker shutdown", result.EndedAt))
		e.m.TasksTotal.WithLabelValues("canceled").Inc()
		e.m.ExecDuration.WithLabelValues("canceled").Observe(result.duration().Seconds())

	case result.TimedOut:
		// Per-task timeout exceeded.
		failed = true
		exitCode := result.ExitCode
		reason := fmt.Sprintf("execution timeout after %s", time.Duration(msg.OnRun.TimeoutSeconds)*time.Second)
		e.logger.WarnContext(
			ctx, "executor: task timed out",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sess.ID),
			slog.Int("pid", result.PID),
			slog.Int("timeout_seconds", msg.OnRun.TimeoutSeconds),
			slog.Duration("duration", result.duration()),
		)
		e.publishStatus(ctx, terminalStatus(msg, sess.ID, "failed", &exitCode, reason, result.EndedAt))
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		e.m.ExecDuration.WithLabelValues("failed").Observe(result.duration().Seconds())

	case result.ExitCode != 0:
		// Task 54: non-zero exit code → failure.
		failed = true
		exitCode := result.ExitCode
		reason := fmt.Sprintf("process exited with code %d", exitCode)
		e.logger.InfoContext(
			ctx, "executor: task failed (non-zero exit code)",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sess.ID),
			slog.Int("exit_code", exitCode),
			slog.Int("pid", result.PID),
			slog.Duration("duration", result.duration()),
		)
		e.publishStatus(ctx, terminalStatus(msg, sess.ID, "failed", &exitCode, reason, result.EndedAt))
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		e.m.ExecDuration.WithLabelValues("failed").Observe(result.duration().Seconds())

	default:
		// Successful completion.
		zero := 0
		e.logger.InfoContext(
			ctx, "executor: task succeeded",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sess.ID),
			slog.Int("pid", result.PID),
			slog.Duration("duration", result.duration()),
		)
		e.publishStatus(ctx, terminalStatus(msg, sess.ID, "succeeded", &zero, "", result.EndedAt))
		e.m.TasksTotal.WithLabelValues("succeeded").Inc()
		e.m.ExecDuration.WithLabelValues("succeeded").Observe(result.duration().Seconds())
	}
}

// ── execProcess ──────────────────────────────────────────────────────────────

// execProcess starts the OS process for msg.OnRun, reads its output, and
// waits for it to exit (or timeout/cancel).  It returns only after the process
// has exited and all output has been consumed.
//
// lookup contains the resolved-mode path map parsed by the caller; it is
// applied to the OnRun command and args before the process is started (task 60).
//
// Callers must check [processResult.Err] for start/wait failures and
// [processResult.TimedOut] / [processResult.Canceled] for forced termination.
func (e *Executor) execProcess(ctx context.Context, msg *protocol.AssignMsg, sess *session.Session, run *taskRun, lookup *pathmap.Lookup) processResult {
	// Task 60: apply resolved-mode path substitution to the command and args
	// so the launched process sees only concrete filesystem paths.
	action := lookup.ApplyToAction(msg.OnRun)

	// Task 50: build process environment.
	// Task 51: set working directory to the session working directory.
	// Task 52: capture stdout and stderr via explicit pipes (not inheriting
	//          the worker's own fds) so output can be attributed and forwarded.
	// exec.Command is used intentionally instead of exec.CommandContext (noctx)
	// because CommandContext automatically sends SIGKILL when ctx is canceled,
	// bypassing the SIGTERM → grace period → SIGKILL escalation required by
	// task 55.  We manage the process lifetime explicitly via killAndWait.
	cmd := exec.Command(action.Command, action.Args...) //nolint:gosec,noctx // command from server-signed assignment; context handled manually
	cmd.Dir = sess.WorkDir
	cmd.Env = buildTaskEnv(msg)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return processResult{Err: fmt.Errorf("stdout pipe: %w", err)}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return processResult{Err: fmt.Errorf("stderr pipe: %w", err)}
	}

	// Task 53: record start time.
	startedAt := time.Now()

	// Publish "running" status before Start so the server records the attempt
	// even if the command is not found or cannot be executed.  If Start fails
	// the caller will publish a terminal "failed" immediately after, giving the
	// server a well-formed running→failed transition rather than an orphaned
	// "failed" with no preceding "running" (task 77).
	e.publishStatus(ctx, runningStatus(msg, sess.ID, startedAt))

	if err := cmd.Start(); err != nil {
		return processResult{
			Err:       fmt.Errorf("start %q: %w", action.Command, err),
			StartedAt: startedAt,
			EndedAt:   time.Now(),
		}
	}

	// Task 53: record PID.
	pid := cmd.Process.Pid
	run.pid = pid
	run.startedAt = startedAt

	// Track this task ID in the session so Session.ActiveTaskCount() is accurate.
	sess.AddTask(msg.TaskID)
	defer sess.RemoveTask(msg.TaskID)

	// Task 52: read stdout and stderr concurrently to prevent pipe-buffer
	// deadlocks (if one pipe fills up waiting for the other to be drained,
	// both sides block indefinitely).
	var readWg sync.WaitGroup
	readWg.Add(2)
	go func() {
		defer readWg.Done()
		scanOutput(ctx, stdoutPipe, "stdout", msg.TaskID, msg.AttemptID, sess.ID, e.outputHandler, e.logger)
	}()
	go func() {
		defer readWg.Done()
		scanOutput(ctx, stderrPipe, "stderr", msg.TaskID, msg.AttemptID, sess.ID, e.outputHandler, e.logger)
	}()

	// Wait for the output goroutines to finish and then for cmd.Wait() to
	// return.  cmd.Wait() must be called after all pipe reads complete (Go
	// docs: "it is incorrect to call Wait before all reads from the pipe have
	// completed").
	waitDone := make(chan error, 1)
	go func() {
		readWg.Wait()
		waitDone <- cmd.Wait()
	}()

	// Task 55: compute effective timeout.
	var taskTimeout time.Duration
	if action.TimeoutSeconds > 0 {
		taskTimeout = time.Duration(action.TimeoutSeconds) * time.Second
	}

	if taskTimeout > 0 {
		return e.waitWithTimeout(ctx, cmd, run, waitDone, startedAt, pid, taskTimeout)
	}
	return e.waitForCompletion(ctx, cmd, run, waitDone, startedAt, pid)
}

// ── Wait helpers ──────────────────────────────────────────────────────────────

// waitForCompletion blocks until the process exits normally or ctx is canceled.
func (e *Executor) waitForCompletion(
	ctx context.Context,
	cmd *exec.Cmd,
	run *taskRun,
	waitDone <-chan error,
	startedAt time.Time,
	pid int,
) processResult {
	select {
	case waitErr := <-waitDone:
		return makeResult(waitErr, pid, startedAt, false, false)
	case <-ctx.Done():
		return e.killAndWait(ctx, cmd, run, waitDone, startedAt, pid, false, true)
	}
}

// waitWithTimeout blocks until the process exits, the per-task timeout
// elapses, or ctx is canceled — whichever comes first.
func (e *Executor) waitWithTimeout(
	ctx context.Context,
	cmd *exec.Cmd,
	run *taskRun,
	waitDone <-chan error,
	startedAt time.Time,
	pid int,
	timeout time.Duration,
) processResult {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case waitErr := <-waitDone:
		return makeResult(waitErr, pid, startedAt, false, false)
	case <-timer.C:
		return e.killAndWait(ctx, cmd, run, waitDone, startedAt, pid, true, false)
	case <-ctx.Done():
		return e.killAndWait(ctx, cmd, run, waitDone, startedAt, pid, false, true)
	}
}

// killAndWait sends SIGTERM to the process, waits up to [Config.KillGracePeriod]
// for it to exit, then escalates to SIGKILL.  It blocks until cmd.Wait()
// returns (which happens only after all pipe-reading goroutines exit).
func (e *Executor) killAndWait(
	ctx context.Context,
	cmd *exec.Cmd,
	run *taskRun,
	waitDone <-chan error,
	startedAt time.Time,
	pid int,
	timedOut bool,
	canceled bool,
) processResult {
	e.logger.WarnContext(
		ctx, "executor: terminating process",
		slog.String("task_id", run.taskID),
		slog.Int("pid", pid),
		slog.Bool("timed_out", timedOut),
		slog.Bool("canceled", canceled),
	)

	// Task 55: SIGTERM first.
	if err := sendTERM(cmd.Process); err != nil {
		e.logger.WarnContext(
			ctx, "executor: SIGTERM failed — escalating to SIGKILL immediately",
			slog.String("task_id", run.taskID),
			slog.Int("pid", pid),
			slog.Any("error", err),
		)
		if killErr := sendKILL(cmd.Process); killErr != nil {
			e.logger.WarnContext(
				ctx, "executor: SIGKILL failed",
				slog.String("task_id", run.taskID),
				slog.Int("pid", pid),
				slog.Any("error", killErr),
			)
		}
		waitErr := <-waitDone
		return makeResult(waitErr, pid, startedAt, timedOut, canceled)
	}

	// Wait for the process to exit gracefully or escalate to SIGKILL.
	// Use time.NewTimer (not time.After) so the timer is stopped and GC'd
	// promptly when the process exits before the grace period elapses (B5).
	graceTimer := time.NewTimer(e.cfg.KillGracePeriod)
	defer graceTimer.Stop()
	select {
	case waitErr := <-waitDone:
		return makeResult(waitErr, pid, startedAt, timedOut, canceled)
	case <-graceTimer.C:
		e.logger.WarnContext(
			ctx, "executor: process did not exit after SIGTERM — sending SIGKILL",
			slog.String("task_id", run.taskID),
			slog.Int("pid", pid),
			slog.Duration("grace_period", e.cfg.KillGracePeriod),
		)
		if killErr := sendKILL(cmd.Process); killErr != nil {
			e.logger.WarnContext(
				ctx, "executor: SIGKILL failed",
				slog.String("task_id", run.taskID),
				slog.Int("pid", pid),
				slog.Any("error", killErr),
			)
		}
		waitErr := <-waitDone
		return makeResult(waitErr, pid, startedAt, timedOut, canceled)
	}
}

// makeResult converts a raw cmd.Wait() error into a structured [processResult].
func makeResult(waitErr error, pid int, startedAt time.Time, timedOut, canceled bool) processResult {
	endedAt := time.Now()
	exitCode := 0
	var unexpectedErr error

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		// Clean exit.
	case errors.As(waitErr, &exitErr):
		// Non-zero exit code or signal termination.
		exitCode = exitErr.ExitCode()
		// exitErr.ExitCode() returns -1 when the process was killed by a
		// signal.  That is expected when timedOut or canceled.
	default:
		// Unexpected wait failure (e.g., process not found, OS error).
		unexpectedErr = waitErr
		exitCode = -1
	}

	return processResult{
		ExitCode:  exitCode,
		PID:       pid,
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Err:       unexpectedErr,
		TimedOut:  timedOut,
		Canceled:  canceled,
	}
}

// ── Output capture ────────────────────────────────────────────────────────────

// scanOutput reads lines from r and forwards each to handler.HandleLine.
// It returns when r reaches EOF (the process has closed the pipe write end,
// which happens when the process exits).
//
// Task 52: stdout and stderr are read in separate goroutines to prevent pipe
// deadlocks.
func scanOutput(
	ctx context.Context,
	r io.Reader,
	stream, taskID, attemptID, sessionID string,
	handler OutputHandler,
	logger *slog.Logger,
) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		handler.HandleLine(ctx, taskID, attemptID, sessionID, stream, scanner.Text())
	}
	// scanner.Err() returns nil on EOF (normal completion) and a non-nil error
	// on unexpected read failures.  A non-nil error here typically means the
	// process was killed and the pipe was closed abruptly; log at debug level
	// to avoid alarming operators on expected termination paths.
	if err := scanner.Err(); err != nil {
		// Use a background context in case ctx is already canceled.
		logger.DebugContext(
			context.Background(), "executor: output scan error",
			slog.String("task_id", taskID),
			slog.String("stream", stream),
			slog.Any("error", err),
		)
	}
}

// ── Environment construction ─────────────────────────────────────────────────

// buildTaskEnv builds the environment variable slice for the task process
// (task 50).
//
// Strategy:
//  1. Start with os.Environ() so DCC tools find their expected system
//     variables (PATH, library dirs, HOME, etc.).
//  2. Merge in the Variables from each AssignEnvironment in declaration order;
//     later environments' variables take precedence over earlier ones.
//
// The resulting slice has the format expected by exec.Cmd.Env ("KEY=VALUE").
// Map iteration order is non-deterministic; callers must not rely on the
// ordering of variables within the slice.
func buildTaskEnv(msg *protocol.AssignMsg) []string {
	// Collect all per-environment variables.  Later entries win.
	var taskVars map[string]string
	for _, env := range msg.Environments {
		if len(env.Variables) == 0 {
			continue
		}
		if taskVars == nil {
			taskVars = make(map[string]string, len(env.Variables))
		}
		maps.Copy(taskVars, env.Variables)
	}

	base := os.Environ()
	if len(taskVars) == 0 {
		return base
	}

	// Index the inherited environment for O(n) override application.
	merged := make(map[string]string, len(base)+len(taskVars))
	for _, kv := range base {
		k, v := splitEnvPair(kv)
		merged[k] = v
	}
	// Task variables take precedence over the inherited environment.
	maps.Copy(merged, taskVars)

	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// splitEnvPair splits "KEY=VALUE" into ("KEY", "VALUE").
// If there is no '=' the key is the entire string and the value is empty.
func splitEnvPair(kv string) (key, value string) {
	for i := range len(kv) {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:]
		}
	}
	return kv, ""
}
