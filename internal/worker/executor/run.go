// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
)

// ── processResult ─────────────────────────────────────────────────────────────

// processResult captures the complete outcome of an OS process execution
// (PID, start time, end time, exit code).
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
	// OnRun.TimeoutSeconds was exceeded.
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
//  3. Publish "running" status (included here as integral to the executor).
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
	// Signal the WaitGroup when this goroutine exits so DrainAndShutdown can
	// block until all goroutines have published their terminal statuses (tasks
	// 86–87).  Registered first so it runs last in LIFO order — after all
	// other cleanup defers have completed.
	defer e.wg.Done()

	// failed controls the keepFailedSessions behavior in Cleanup.
	failed := false

	// Per-task derived context for execution control.  Derived from e.execCtx
	// (not from the worker's signal ctx) so the task goroutine survives
	// SIGINT/SIGTERM and is only killed when DrainAndShutdown cancels execCtx
	// after the shutdown grace period expires.
	//
	// openjd_fail and per-task server cancels call taskCancel directly, which
	// only cancels this specific task without touching other tasks or execCtx.
	taskCtx, taskCancel := context.WithCancel(e.execCtx)
	defer taskCancel()

	// Wire the per-task cancel into the OpenJD interceptor, if wired in.
	if hook, ok := e.outputHandler.(TaskLifecycleHook); ok {
		hook.RegisterTask(msg.AttemptID, msg.TaskID, msg.JobID, sess.ID, taskCancel)
		defer hook.Deregister(msg.AttemptID)
	}

	// ── Store cancel function ───────────────────────────────────────
	// applyCancelFunc stores taskCancel and fires it immediately if Cancel() was
	// called before this goroutine reached here.
	e.applyCancelFunc(run, taskCancel)

	// LIFO defers — registration order (first→last) and execution order (last→first):
	//   Registered 1st → runs last:  wg.Done
	//   Registered 2nd → runs 4th:   taskCancel
	//   Registered 3rd → runs 3rd:   hook.Deregister (if hook is wired)
	//   Registered 4th → runs 2nd:   sessionMgr.Cleanup
	//   Registered 5th → runs 1st:   removeActiveTask
	//   Registered 6th → runs 0th:   registerCancelSubscription cleanup
	//
	// Deregistering the cancel subscription first ensures no cancel signal can
	// arrive after the task is gone from activeTasks.  wg.Done runs last so
	// DrainAndShutdown.wg.Wait() returns only after all cleanup is complete.
	defer func() { e.sessionMgr.Cleanup(context.Background(), sess, failed) }()
	defer e.removeActiveTask(run.taskID)
	// Register per-task cancel subscription; cleanup Deregisters when the task
	// exits (runs before removeActiveTask in LIFO order).
	defer e.registerCancelSubscription(ctx, msg.TaskID)()

	// ── Nil-action guard ──────────────────────────────────────────────────────
	if msg.OnRun == nil {
		e.logger.WarnContext(
			ctx, "executor: assignment has no OnRun action — treating as succeeded",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
		)
		e.statusPub.Running(context.Background(), msg, sess.ID, nil, time.Now())
		zero := 0
		e.statusPub.Terminal(context.Background(), msg, sess.ID, "succeeded", &zero, "", nil, time.Now())
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
		e.statusPub.Running(context.Background(), msg, sess.ID, nil, time.Now())
		minusOne := -1
		e.statusPub.Terminal(context.Background(), msg, sess.ID, "failed", &minusOne,
			"task has an empty command", nil, time.Now())
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		return
	}

	// ── Validate and parse path map ────────────────────────────
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
		e.failPreExec(msg, sess.ID, &failed, err.Error())
		return
	}

	// ── OpenJD path mapping file ────────────────────────────────────
	// The pathmapping-1.0 path_mapping.json file is written once at session
	// creation (session.Manager.Create) so that both environment actions and the
	// task action can rely on Session.PathMappingRulesFile.  It is intentionally
	// NOT written here, to avoid a double-write.

	// ── Resolve OpenJD {{...}} format strings ──────────────────────────────────
	// The session working directory (Session.WorkingDirectory) is only known now,
	// at run time, so command/args, environment-variable values, and embedded
	// file data are resolved here on the worker rather than server-side.  A bad
	// reference fails the task cleanly with a descriptive message naming the
	// offending variable.  resolvedFiles are the step embedded files with their
	// data resolved against the task scope (which includes Task.File.*).
	resolvedRun, resolvedEnvVars, resolvedFiles, err := resolveAssignment(msg, sess)
	if err != nil {
		e.failResolution(ctx, msg, sess, &failed, err)
		return
	}

	// ── Materialize step-level embedded files ────────────────────────────────
	// Files are written into the session working directory before OnRun so the
	// task command can reference them by name (and by {{Task.File.<name>}}).
	// Environment-level embedded files are materialized earlier, at
	// environment-enter time (session.enterOne).  Data was resolved above against
	// the task scope; the writer materializes the already-resolved copies.
	if err := sess.WriteEmbeddedFiles(resolvedFiles); err != nil {
		e.logger.ErrorContext(
			ctx, "executor: step embedded file write failed — failing task",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.Any("error", err),
		)
		e.failPreExec(msg, sess.ID, &failed, err.Error())
		return
	}

	// ── Execute process ───────────────────────────────────────────────────────
	result := e.execProcess(taskCtx, msg, sess, run, lookup, resolvedRun, resolvedEnvVars)

	// ── Flush buffered log output ───────────────────────────────────
	e.flushTaskLogs(ctx, msg, sess.ID)

	// ── Retrieve openjd_fail reason ────────────────────────────────
	// Must be called inline (before deferred Deregister runs) so the stored
	// reason is available for the terminal status switch below.
	// Both the reason and the bool are kept: openjdFailed=true when openjd_fail
	// was seen, even if the emitted reason text was empty.
	var openjdFailReason string
	var openjdFailed bool
	if hook, ok := e.outputHandler.(TaskLifecycleHook); ok {
		openjdFailReason, openjdFailed = hook.TakeFailReason(msg.AttemptID)
	}

	// ── Snapshot last-known progress ────────────────────────────────
	// Must be called before deferred Deregister removes the attempt state from
	// the openjd interceptor.
	lp := e.lastProgress(msg.AttemptID)

	// ── Publish terminal status and update metrics ────────────────────────────
	// Cases are ordered so that openjdFailed and the worker-shutdown paths take
	// priority.  result.Err (unexpected start/wait failure) is checked after
	// Canceled and TimedOut because makeResult only sets Err for non-ExitError
	// failures, which cannot co-occur with timeout or cancellation.
	switch {
	case openjdFailed:
		// The task self-reported failure via openjd_fail.  The process was
		// terminated via taskCancel using the action's cancelation method
		// (TERMINATE by default → immediate SIGKILL); treat the terminal
		// state as "failed" with the process-supplied reason rather than
		// "canceled" so the server records the correct outcome.
		// openjdFailReason may be an empty string when the process emitted
		// "openjd_fail:" with no accompanying text.
		failed = true
		exitCode := result.ExitCode
		e.logger.WarnContext(
			ctx, "executor: task failed (openjd_fail)",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sess.ID),
			slog.String("reason", openjdFailReason),
			slog.Int("pid", result.PID),
			slog.Duration("duration", result.duration()),
		)
		e.statusPub.Terminal(context.Background(), msg, sess.ID, "failed", &exitCode, openjdFailReason, lp, result.EndedAt)
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		e.m.ExecDuration.WithLabelValues("failed").Observe(result.duration().Seconds())

	case result.Canceled:
		// Task was killed due to context cancellation.  If DrainAndShutdown set
		// shuttingDown before canceling execCtx, this is a worker force-shutdown:
		// publish "failed"/"worker_shutdown". Otherwise it is a
		// per-task server cancel: publish "canceled".
		failed = true
		if e.shuttingDown.Load() {
			// Worker force-shutdown path.
			e.logger.InfoContext(
				ctx, "executor: task failed (worker shutdown)",
				slog.String("task_id", msg.TaskID),
				slog.String("attempt_id", msg.AttemptID),
				slog.String("session_id", sess.ID),
				slog.Int("pid", result.PID),
				slog.Duration("duration", result.duration()),
			)
			e.statusPub.Terminal(context.Background(), msg, sess.ID, "failed", nil, "worker_shutdown", lp, result.EndedAt)
			e.m.TasksTotal.WithLabelValues("failed").Inc()
			e.m.ExecDuration.WithLabelValues("failed").Observe(result.duration().Seconds())
		} else {
			// Per-task server cancel.
			e.logger.InfoContext(
				ctx, "executor: task canceled",
				slog.String("task_id", msg.TaskID),
				slog.String("attempt_id", msg.AttemptID),
				slog.String("session_id", sess.ID),
				slog.Int("pid", result.PID),
				slog.Duration("duration", result.duration()),
			)
			e.statusPub.Terminal(context.Background(), msg, sess.ID, "canceled", nil, "", lp, result.EndedAt)
			e.m.TasksTotal.WithLabelValues("canceled").Inc()
			e.m.ExecDuration.WithLabelValues("canceled").Observe(result.duration().Seconds())
		}

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
		e.statusPub.Terminal(context.Background(), msg, sess.ID, "failed", &exitCode, reason, lp, result.EndedAt)
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		e.m.ExecDuration.WithLabelValues("failed").Observe(result.duration().Seconds())

	case result.Err != nil:
		// Process failed to start or produced an unexpected wait error.
		// Checked after Canceled/TimedOut because makeResult only sets Err
		// for non-ExitError failures — SIGTERM/SIGKILL termination returns an
		// ExitError, so Err is nil in those paths.
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
		e.statusPub.Terminal(context.Background(), msg, sess.ID, "failed", &exitCode, reason, lp, result.EndedAt)
		e.m.TasksTotal.WithLabelValues("failed").Inc()
		e.m.ExecDuration.WithLabelValues("failed").Observe(result.duration().Seconds())

	case result.ExitCode != 0:
		// Non-zero exit code → failure.
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
		e.statusPub.Terminal(context.Background(), msg, sess.ID, "failed", &exitCode, reason, lp, result.EndedAt)
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
		e.statusPub.Terminal(context.Background(), msg, sess.ID, "succeeded", &zero, "", lp, result.EndedAt)
		e.m.TasksTotal.WithLabelValues("succeeded").Inc()
		e.m.ExecDuration.WithLabelValues("succeeded").Observe(result.duration().Seconds())
	}
}

// resolveAssignment resolves the OpenJD {{...}} format strings for a task:
//   - the OnRun action against the task-action scope (Task.Param.*, Task.File.*,
//     Param.*, Session.*);
//   - each step embedded file's data against that same task scope, so file data
//     may reference Task.Param.* and Task.File.* (the materialized path of any
//     step file, including itself or a sibling);
//   - the merged environment-variable values against the environment scope
//     (Task.Param.* intentionally unavailable; Env.File.* of every environment
//     exposed so env-var values may reference an environment file's path).
//
// It returns the resolved action copy, the resolved environment-variable map,
// and the step embedded files with their data resolved, or a descriptive error
// naming the offending reference.
func resolveAssignment(msg *protocol.AssignMsg, sess *session.Session) (*protocol.Action, map[string]string, []protocol.EmbeddedFile, error) {
	workDir := sess.WorkDir
	pathMapFile := sess.PathMappingRulesFile()
	hasPathMap := sess.HasPathMappingRules()

	taskScope := fmtres.TaskScope(msg.JobParameters, msg.Parameters, workDir, pathMapFile, hasPathMap)
	// Expose Task.File.<name> -> materialized path for each step embedded file
	// BEFORE resolving the command/args and the files' own data, so both can
	// reference a file's on-disk path.
	if err := fmtres.AddFileVars(taskScope, "Task.File", msg.EmbeddedFiles, workDir); err != nil {
		return nil, nil, nil, fmt.Errorf("step embedded files: %w", err)
	}
	resolvedRun, err := fmtres.ResolveAction(msg.OnRun, taskScope)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve command: %w", err)
	}
	resolvedFiles, err := fmtres.ResolveEmbeddedFiles(msg.EmbeddedFiles, taskScope)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve embedded file data: %w", err)
	}

	// Reuse the static environment that session.Create already resolved at
	// enter time. Each environment's Variables were resolved against that
	// environment's OWN Env.File.* scope; re-resolving here against a merged
	// all-environments scope would both duplicate the work and risk resolving a
	// same-named Env.File.* reference to a different file (last-wins across
	// environments). The session is the single source of truth.
	resolvedEnvVars := sess.StaticEnv()
	return resolvedRun, resolvedEnvVars, resolvedFiles, nil
}

// failPreExec publishes the running→failed transition for a pre-execution
// failure (exit code -1) and marks the task failed. Callers log their own
// context-specific message before calling.
func (e *Executor) failPreExec(msg *protocol.AssignMsg, sessID string, failed *bool, reason string) {
	*failed = true
	e.statusPub.Running(context.Background(), msg, sessID, nil, time.Now())
	minusOne := -1
	e.statusPub.Terminal(context.Background(), msg, sessID, "failed", &minusOne, reason, nil, time.Now())
	e.m.TasksTotal.WithLabelValues("failed").Inc()
}

// failResolution publishes the running→failed transition for a task that could
// not have its OpenJD format strings resolved, mirroring the other
// pre-execution failure paths in runTask.  It sets *failed so the deferred
// session cleanup honors keepFailedSessions, and records the failure metric.
func (e *Executor) failResolution(ctx context.Context, msg *protocol.AssignMsg, sess *session.Session, failed *bool, err error) {
	e.logger.ErrorContext(
		ctx, "executor: format-string resolution failed — failing task",
		slog.String("task_id", msg.TaskID),
		slog.String("attempt_id", msg.AttemptID),
		slog.Any("error", err),
	)
	e.failPreExec(msg, sess.ID, failed, err.Error())
}

// ── execProcess ──────────────────────────────────────────────────────────────

// execProcess starts the OS process for msg.OnRun, reads its output, and
// waits for it to exit (or timeout/cancel).  It returns only after the process
// has exited and all output has been consumed.
//
// lookup contains the resolved-mode path map parsed by the caller; it is
// applied to the OnRun command and args before the process is started.
//
// Callers must check [processResult.Err] for start/wait failures and
// [processResult.TimedOut] / [processResult.Canceled] for forced termination.
// resolvedRun is the OnRun action with its format strings already resolved by
// the caller; envVars is the merged, format-string-resolved environment-variable
// map to apply on top of the inherited process environment.
func (e *Executor) execProcess(ctx context.Context, msg *protocol.AssignMsg, sess *session.Session, run *taskRun, lookup *pathmap.Lookup, resolvedRun *protocol.Action, envVars map[string]string) processResult {
	// Apply resolved-mode path substitution to the command and args
	// so the launched process sees only concrete filesystem paths.
	action := lookup.ApplyToAction(resolvedRun)

	// Build process environment.
	// Set working directory to the session working directory.
	// Capture stdout and stderr via explicit pipes (not inheriting
	//          the worker's own fds) so output can be attributed and forwarded.
	// exec.Command is used intentionally instead of exec.CommandContext (noctx)
	// because CommandContext sends a fixed signal when ctx is canceled, bypassing
	// the OpenJD Action.cancelation policy (TERMINATE vs NOTIFY_THEN_TERMINATE)
	// that killAndWait applies.  We manage the process lifetime explicitly.
	cmd := exec.Command(action.Command, action.Args...) //nolint:gosec,noctx // command from server-signed assignment; context handled manually
	cmd.Dir = sess.WorkDir

	// Merge the session's dynamic environment (accumulated from openjd_env /
	// openjd_redacted_env / openjd_unset_env directives emitted by environment
	// actions) on top of the task's static environment variables. Precedence:
	// worker os.Environ (lowest) < static env Variables < session dynamic env;
	// openjd_unset_env removes a variable even if a static entry or the worker
	// environment set it.
	overrides := envVars
	if dyn := sess.EnvOverrides(); len(dyn) > 0 {
		overrides = make(map[string]string, len(envVars)+len(dyn))
		maps.Copy(overrides, envVars)
		maps.Copy(overrides, dyn)
	}
	cmd.Env = envutil.BuildWithUnset(overrides, sess.EnvUnset())

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return processResult{Err: fmt.Errorf("stdout pipe: %w", err)}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return processResult{Err: fmt.Errorf("stderr pipe: %w", err)}
	}

	// Record start time.
	startedAt := time.Now()

	// Publish "running" status before Start so the server records the attempt
	// even if the command is not found or cannot be executed.  If Start fails
	// the caller will publish a terminal "failed" immediately after, giving the
	// server a well-formed running→failed transition rather than an orphaned
	// "failed" with no preceding "running".
	// lastProgress is nil here because the process has not yet emitted any output.
	e.statusPub.Running(context.Background(), msg, sess.ID, nil, startedAt)

	if err := cmd.Start(); err != nil {
		return processResult{
			Err:       fmt.Errorf("start %q: %w", action.Command, err),
			StartedAt: startedAt,
			EndedAt:   time.Now(),
		}
	}

	// Record PID.
	pid := cmd.Process.Pid
	run.pid = pid
	run.startedAt = startedAt

	// Track this task ID in the session so Session.ActiveTaskCount() is accurate.
	sess.AddTask(msg.TaskID)
	defer sess.RemoveTask(msg.TaskID)

	// Read stdout and stderr concurrently to prevent pipe-buffer
	// deadlocks (if one pipe fills up waiting for the other to be drained,
	// both sides block indefinitely).
	var readWg sync.WaitGroup
	readWg.Add(2)
	go func() {
		defer readWg.Done()
		scanOutput(ctx, stdoutPipe, "stdout", msg.TaskID, msg.AttemptID, sess.ID, e.outputHandler, sess.ScrubRedacted, e.logger)
	}()
	go func() {
		defer readWg.Done()
		scanOutput(ctx, stderrPipe, "stderr", msg.TaskID, msg.AttemptID, sess.ID, e.outputHandler, sess.ScrubRedacted, e.logger)
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

	// Compute effective timeout.
	var taskTimeout time.Duration
	if action.TimeoutSeconds > 0 {
		taskTimeout = time.Duration(action.TimeoutSeconds) * time.Second
	}

	// The OpenJD Action.cancelation method governs how the process is terminated
	// on timeout or cancellation (TERMINATE = immediate SIGKILL, the spec default;
	// NOTIFY_THEN_TERMINATE = SIGTERM then SIGKILL after the notify period).
	if taskTimeout > 0 {
		return e.waitWithTimeout(ctx, cmd, run, waitDone, startedAt, pid, taskTimeout, action.Cancelation)
	}
	return e.waitForCompletion(ctx, cmd, run, waitDone, startedAt, pid, action.Cancelation)
}

// ── Wait helpers ──────────────────────────────────────────────────────────────

// waitForCompletion blocks until the process exits normally or ctx is canceled.
// cancelation is the OpenJD Action.cancelation method honored when the process
// must be forcibly terminated.
func (e *Executor) waitForCompletion(
	ctx context.Context,
	cmd *exec.Cmd,
	run *taskRun,
	waitDone <-chan error,
	startedAt time.Time,
	pid int,
	cancelation *protocol.CancelationMethod,
) processResult {
	select {
	case waitErr := <-waitDone:
		return makeResult(waitErr, pid, startedAt, false, false)
	case <-ctx.Done():
		return e.killAndWait(ctx, cmd, run, waitDone, startedAt, pid, false, true, cancelation)
	}
}

// waitWithTimeout blocks until the process exits, the per-task timeout
// elapses, or ctx is canceled — whichever comes first.  cancelation is the
// OpenJD Action.cancelation method honored when the process must be forcibly
// terminated (on timeout or cancellation).
func (e *Executor) waitWithTimeout(
	ctx context.Context,
	cmd *exec.Cmd,
	run *taskRun,
	waitDone <-chan error,
	startedAt time.Time,
	pid int,
	timeout time.Duration,
	cancelation *protocol.CancelationMethod,
) processResult {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case waitErr := <-waitDone:
		return makeResult(waitErr, pid, startedAt, false, false)
	case <-timer.C:
		return e.killAndWait(ctx, cmd, run, waitDone, startedAt, pid, true, false, cancelation)
	case <-ctx.Done():
		return e.killAndWait(ctx, cmd, run, waitDone, startedAt, pid, false, true, cancelation)
	}
}

// OpenJD Action.cancelation modes and notify-period bounds.
//
// These implement the OpenJD spec for how a running action is terminated on
// cancel/timeout.  TERMINATE is the spec default when no cancelation is
// specified: terminate immediately (SIGKILL on POSIX) with no SIGTERM grace.
const (
	// modeTerminate terminates the action immediately — SIGKILL on POSIX, no
	// SIGTERM, no grace period.  This is the OpenJD spec default when an action
	// declares no cancelation method.
	modeTerminate = "TERMINATE"
	// modeNotifyThenTerminate sends SIGTERM (notify), waits the effective notify
	// period, then SIGKILL.
	modeNotifyThenTerminate = "NOTIFY_THEN_TERMINATE"

	// defaultNotifyPeriod is the OpenJD default notify period for an OnRun action
	// using NOTIFY_THEN_TERMINATE when notifyPeriodInSeconds is unset (the "30s
	// otherwise" default applies only to non-run actions, which are out of scope
	// here).
	defaultNotifyPeriod = 120 * time.Second
	// maxNotifyPeriod is the OpenJD maximum allowed notify period; larger values
	// are clamped down to this ceiling.
	maxNotifyPeriod = 600 * time.Second
)

// terminationMode resolves the effective cancelation mode and, for
// NOTIFY_THEN_TERMINATE, the effective notify period from an OpenJD
// Action.cancelation method.
//
// A nil method (no cancelation specified) yields TERMINATE — the OpenJD spec
// default — which terminates immediately with no notify period.  Any mode other
// than NOTIFY_THEN_TERMINATE is likewise treated as TERMINATE.
//
// For NOTIFY_THEN_TERMINATE the notify period is NotifyPeriodSeconds when > 0,
// otherwise the 120 s default, clamped to a 600 s maximum.
func terminationMode(c *protocol.CancelationMethod) (mode string, notifyPeriod time.Duration) {
	if c == nil || c.Mode != modeNotifyThenTerminate {
		return modeTerminate, 0
	}
	period := defaultNotifyPeriod
	if c.NotifyPeriodSeconds > 0 {
		period = time.Duration(c.NotifyPeriodSeconds) * time.Second
	}
	if period > maxNotifyPeriod {
		period = maxNotifyPeriod
	}
	return modeNotifyThenTerminate, period
}

// killAndWait terminates the process and blocks until cmd.Wait() returns (which
// happens only after all pipe-reading goroutines exit).
//
// The termination strategy follows the OpenJD Action.cancelation method carried
// by the assignment (cancelation):
//
//   - TERMINATE (or nil — the OpenJD spec default): SIGKILL immediately, with no
//     SIGTERM and no grace period.
//   - NOTIFY_THEN_TERMINATE: SIGTERM (notify), wait the effective notify period,
//     then escalate to SIGKILL.
//
// Worker force-shutdown (DrainAndShutdown → execCtx cancellation) is a separate
// concern: it always uses the graceful SIGTERM → [Config.KillGracePeriod] →
// SIGKILL escalation regardless of the action's cancelation method, so the
// configured shutdown grace window is honored for every in-flight task.
//
// Note: the worker-shutdown-vs-per-task distinction relies on the shuttingDown
// flag being set before execCtx is canceled in DrainAndShutdown.  A concurrent
// external Cancel racing a shutdown is harmless — the server tolerates duplicate
// or short-window terminal status messages (first one wins, subsequent ignored).
func (e *Executor) killAndWait(
	ctx context.Context,
	cmd *exec.Cmd,
	run *taskRun,
	waitDone <-chan error,
	startedAt time.Time,
	pid int,
	timedOut bool,
	canceled bool,
	cancelation *protocol.CancelationMethod,
) processResult {
	// Decide whether to notify (SIGTERM first) and, if so, the grace period
	// before escalating to SIGKILL.
	var notify bool
	var gracePeriod time.Duration
	switch {
	case canceled && e.shuttingDown.Load():
		// Worker force-shutdown: preserve the configured shutdown grace window
		// independent of the per-action cancelation method.
		notify = true
		gracePeriod = e.cfg.KillGracePeriod
	default:
		// Per-task action cancelation (per-task timeout, per-task server cancel,
		// or openjd_fail): honor the OpenJD Action.cancelation method.  TERMINATE
		// (the spec default) kills immediately; NOTIFY_THEN_TERMINATE notifies
		// first and waits the effective notify period.
		mode, period := terminationMode(cancelation)
		notify = mode == modeNotifyThenTerminate
		gracePeriod = period
	}

	e.logger.WarnContext(
		ctx, "executor: terminating process",
		slog.String("task_id", run.taskID),
		slog.String("attempt_id", run.attemptID),
		slog.Int("pid", pid),
		slog.Bool("timed_out", timedOut),
		slog.Bool("canceled", canceled),
		slog.Bool("notify", notify),
		slog.Duration("grace_period", gracePeriod),
	)

	// TERMINATE (default): SIGKILL immediately — no SIGTERM, no grace period.
	if !notify {
		if killErr := sendKILL(cmd.Process); killErr != nil {
			e.logger.WarnContext(
				ctx, "executor: SIGKILL failed",
				slog.String("task_id", run.taskID),
				slog.String("attempt_id", run.attemptID),
				slog.Int("pid", pid),
				slog.Any("error", killErr),
			)
		}
		waitErr := <-waitDone
		return makeResult(waitErr, pid, startedAt, timedOut, canceled)
	}

	// NOTIFY_THEN_TERMINATE (or worker shutdown): SIGTERM first.
	if err := sendTERM(cmd.Process); err != nil {
		e.logger.WarnContext(
			ctx, "executor: SIGTERM failed — escalating to SIGKILL immediately",
			slog.String("task_id", run.taskID),
			slog.String("attempt_id", run.attemptID),
			slog.Int("pid", pid),
			slog.Any("error", err),
		)
		if killErr := sendKILL(cmd.Process); killErr != nil {
			e.logger.WarnContext(
				ctx, "executor: SIGKILL failed",
				slog.String("task_id", run.taskID),
				slog.String("attempt_id", run.attemptID),
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
	graceTimer := time.NewTimer(gracePeriod)
	defer graceTimer.Stop()
	select {
	case waitErr := <-waitDone:
		return makeResult(waitErr, pid, startedAt, timedOut, canceled)
	case <-graceTimer.C:
		e.logger.WarnContext(
			ctx, "executor: process did not exit after SIGTERM — sending SIGKILL",
			slog.String("task_id", run.taskID),
			slog.String("attempt_id", run.attemptID),
			slog.Int("pid", pid),
			slog.Duration("grace_period", gracePeriod),
		)
		if killErr := sendKILL(cmd.Process); killErr != nil {
			e.logger.WarnContext(
				ctx, "executor: SIGKILL failed",
				slog.String("task_id", run.taskID),
				slog.String("attempt_id", run.attemptID),
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

// flushTaskLogs calls FlushLogs on the OutputHandler if it implements
// [LogFlusher], draining buffered lines before the terminal status is
// published. Uses context.Background() so the flush completes
// even when ctx is already canceled (e.g., during worker shutdown).
func (e *Executor) flushTaskLogs(ctx context.Context, msg *protocol.AssignMsg, sessionID string) {
	flusher, ok := e.outputHandler.(LogFlusher)
	if !ok {
		return
	}
	if err := flusher.FlushLogs(context.Background(), msg.TaskID, msg.AttemptID); err != nil {
		e.logger.WarnContext(
			ctx, "executor: log flush failed before terminal status",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
	}
}

// ── Output capture ────────────────────────────────────────────────────────────

// scanOutput reads lines from r, scrubs each through scrub, and forwards the
// result to handler.HandleLine. It returns when r reaches EOF (the process has
// closed the pipe write end, which happens when the process exits).
//
// scrub redacts any openjd_redacted_env values the step may echo to its own
// output; it may be nil to disable scrubbing.
//
// Stdout and stderr are read in separate goroutines to prevent pipe
// deadlocks.
func scanOutput(
	ctx context.Context,
	r io.Reader,
	stream, taskID, attemptID, sessionID string,
	handler OutputHandler,
	scrub func(string) string,
	logger *slog.Logger,
) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if scrub != nil {
			line = scrub(line)
		}
		handler.HandleLine(ctx, taskID, attemptID, sessionID, stream, line)
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
			slog.String("attempt_id", attemptID),
			slog.String("stream", stream),
			slog.Any("error", err),
		)
	}
}
