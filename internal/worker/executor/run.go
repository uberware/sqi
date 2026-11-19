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
	"strings"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/openjd/logprogress"
	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/procout"
	"github.com/uberware/sqi/internal/worker/proctree"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
)

// applyCredential attaches an isolation.Credential to a process before it
// starts. Defaults to isolation.Apply; tests override it to record that the
// task-process launch site attaches a credential without a real OS identity
// switch. A fake cannot verify real uid/gid switching (see isolation.NewFake);
// make test-isolation covers that.
var applyCredential = isolation.Apply

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
	// unexpected wait error).  A non-zero exit code is not reflected in Err;
	// it is captured in ExitCode.  Process termination due to a signal (when
	// TimedOut or Canceled is true) also does not set Err.
	Err error
	// TimedOut is true when the process was killed because the per-task
	// OnRun.TimeoutSeconds was exceeded.
	TimedOut bool
	// Canceled is true when the process was killed because its context was
	// canceled, by a per-task cancel or by the shutdown drain.
	Canceled bool
}

// duration returns the wall-clock execution time of the process.
func (r processResult) duration() time.Duration {
	return r.EndedAt.Sub(r.StartedAt)
}

// ── runTask ───────────────────────────────────────────────────────────────────

// runTask is the goroutine that drives the full lifecycle of a single task:
//
//  1. Stop at once, starting nothing, if a cancel or the shutdown drain
//     already stopped the run.
//  2. Validate OnRun action.
//  3. Build env and start the OS process.
//  4. Publish "running" status.
//  5. Read stdout/stderr concurrently, forwarding to the OutputHandler.
//  6. Honor per-task timeout and context cancellation.
//  7. Publish terminal status (succeeded/failed/canceled).
//  8. Update metrics.
//  9. Remove from active-tasks.
//  10. Clean up the session.
//  11. Release the held attempt.
//
// runTask is launched by [Executor.Dispatch] and runs independently until the
// task terminates. Dispatch registered run and made its context, taskCtx,
// before creating the session, and has already handed taskCancel to run, so a
// cancel and the shutdown drain reach it at every stage; runTask cancels
// taskCtx when it returns. ctx is used only for logging.
func (e *Executor) runTask(
	ctx context.Context, msg *protocol.AssignMsg, sess *session.Session, run *taskRun,
	taskCtx context.Context, taskCancel context.CancelFunc,
) {
	// Signal the WaitGroup when this goroutine exits so DrainAndShutdown can
	// block until all goroutines have published their terminal statuses.
	// Registered first so it runs last in LIFO order, after all other cleanup
	// defers have completed.
	defer e.wg.Done()

	// Registered second so it runs second to last: the attempt stays held
	// through the terminal status publish and every cleanup below, and is
	// released immediately before wg.Done.
	defer e.release(run.attemptID)

	// failed controls the keepFailedSessions behavior in Cleanup.
	failed := false

	// openjd_fail and per-task server cancels call taskCancel directly, which
	// only cancels this specific task without touching other tasks or execCtx.
	defer taskCancel()

	// Register the per-task cancel with the OpenJD interceptor, when the output
	// handler is one.
	if hook, ok := e.outputHandler.(TaskLifecycleHook); ok {
		hook.RegisterTask(msg.AttemptID, msg.TaskID, msg.JobID, sess.ID, taskCancel)
		defer hook.Deregister(msg.AttemptID)
	}

	// Hand the step's SQI_LOG_PROGRESS matcher to the interceptor, if both are
	// present. Never fails the task.
	e.installLogProgress(ctx, msg)

	// Defers run last-registered first:
	//   Registered 1st → runs last:        wg.Done
	//   Registered 2nd → runs next-to-last: release (the held attempt)
	//   Registered 3rd → runs 4th:         taskCancel
	//   Registered 4th → runs 3rd:         hook.Deregister (if hook is wired)
	//   Registered 5th → runs 2nd:         sessionMgr.Cleanup
	//   Registered 6th → runs 1st:         removeActiveTask
	//
	// stager.Cleanup, registered further down, runs before all of these.
	// wg.Done runs last so DrainAndShutdown.wg.Wait() returns only after all
	// cleanup is complete.
	defer func() { e.sessionMgr.Cleanup(context.Background(), sess, failed) }()
	defer e.removeActiveTask(run)

	// ── Stopped before it started ─────────────────────────────────────────────
	// The deferred session cleanup still exits the environments.
	if e.stoppedBeforeStart(ctx, msg, sess.ID, taskCtx, &failed) {
		return
	}

	// ── Nil-action guard and command validation ─────────────────────────────────
	if e.endedByOnRunGuard(ctx, msg, sess.ID, &failed) {
		return
	}

	// ── Stage inputs + build effective path-map lookup ──────────────────────────
	// buildEffectiveLookup handles stage_locally copy-in (if enabled), constructs
	// the combined lookup (staging rules prepended so they take precedence), and
	// rewrites path_mapping.json when staging rules are added alongside the
	// translation_file delivery.
	//
	// Pre-execution failures below publish a "running" status immediately
	// before the terminal "failed", as the nil-action and empty-command
	// branches above and execProcess do. The server's state machine needs a
	// running→terminal transition for every attempt; the running status does
	// not mean a process exists.
	lookup, scratchDir, ok := e.buildEffectiveLookup(ctx, msg, sess.ID, sess.WorkDir, sess.Credential(), &failed)
	defer e.stager.Cleanup(scratchDir)
	if !ok {
		return
	}

	// ── OpenJD path mapping file ────────────────────────────────────
	// The pathmapping-1.0 path_mapping.json file is written once at session
	// creation (session.Manager.Create) for the base path-map rules.  When
	// staging is active and translation_file is enabled, buildEffectiveLookup
	// rewrites the file with the combined (staging-prepended) rules above.

	// ── Resolve OpenJD {{...}} format strings ──────────────────────────────────
	// The session working directory (Session.WorkingDirectory) is known only at
	// run time, so command/args, environment-variable values, and embedded
	// file data are resolved here on the worker rather than server-side.  A bad
	// reference fails the task cleanly with a descriptive message naming the
	// offending variable.  resolvedFiles are the step embedded files with their
	// data resolved against the task scope (which includes Task.File.*).
	resolvedRun, resolvedEnvVars, resolvedFiles, err := resolveAssignment(msg, sess)
	if err != nil {
		e.failResolution(ctx, msg, sess, &failed, err)
		return
	}

	// ── Pre-exec validation: s3:// guard + embedded file write ─────────────────
	// Guards the object-store path (see guardUnstagedS3) and materializes
	// step-level embedded files before OnRun.
	// It also compiles the step's SQI_LOG_CHECKS, if any, into the checker that
	// watches the process's output.
	checker, ok := e.preExecValidate(ctx, msg, sess, resolvedRun, resolvedEnvVars, resolvedFiles, &failed)
	if !ok {
		return
	}

	// ── Execute process ───────────────────────────────────────────────────────
	result := e.execProcess(taskCtx, msg, sess, run, lookup, resolvedRun, resolvedEnvVars, checker.wrap(e.outputHandler))

	// ── Flush buffered log output ───────────────────────────────────
	e.flushTaskLogs(ctx, msg, sess.ID)

	// ── Retrieve openjd_fail reason ────────────────────────────────
	// Must be called inline (before deferred Deregister runs) so the stored
	// reason is available for the terminal status switch below.
	// Both the reason and the bool are kept: openjdFailed=true when openjd_fail
	// was seen, even if the emitted reason text was empty.
	openjdFailReason, openjdFailed := e.takeFailReason(msg.AttemptID)

	// ── Snapshot last-known progress ────────────────────────────────
	// Publishes any pending progress snapshot now, after the output readers
	// have drained, so it precedes the terminal status and none can follow it.
	// Must run before deferred Deregister removes the attempt state from the
	// openjd interceptor.
	lp := e.finishProgress(msg.AttemptID)

	// The checker's verdict only matters to a task the cases below would
	// otherwise mark succeeded; it sits after the exit-code case for that reason.
	checkReason, checkFailed := checker.verdict()

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
		e.publishTerminal(msg, sess.ID, "failed", &exitCode, openjdFailReason, lp, result)

	case result.Canceled:
		// Task was killed due to context cancellation: by the shutdown drain
		// ("failed"/"worker_shutdown") or by a per-task cancel ("canceled").
		failed = true
		status, reason := e.stopOutcome()
		e.logger.InfoContext(
			ctx, "executor: task stopped",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sess.ID),
			slog.String("status", status),
			slog.String("reason", reason),
			slog.Int("pid", result.PID),
			slog.Duration("duration", result.duration()),
		)
		e.publishTerminal(msg, sess.ID, status, nil, reason, lp, result)

	case result.TimedOut:
		// Per-task timeout exceeded.
		failed = true
		exitCode := result.ExitCode
		reason := protocol.TimeoutReason(time.Duration(msg.OnRun.TimeoutSeconds) * time.Second)
		e.logger.WarnContext(
			ctx, "executor: task timed out",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("session_id", sess.ID),
			slog.Int("pid", result.PID),
			slog.Int("timeout_seconds", msg.OnRun.TimeoutSeconds),
			slog.Duration("duration", result.duration()),
		)
		e.publishTerminal(msg, sess.ID, "failed", &exitCode, reason, lp, result)

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
		e.publishTerminal(msg, sess.ID, "failed", &exitCode, reason, lp, result)

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
		e.publishTerminal(msg, sess.ID, "failed", &exitCode, reason, lp, result)

	case checkFailed:
		// The process would have succeeded, but its output failed the step's
		// SQI_LOG_CHECKS. Every case above takes precedence, and outputs are not
		// staged out: a failed check says the task's outputs cannot be trusted.
		failed = true
		e.failLogChecks(ctx, msg, sess.ID, result, checkReason, lp)

	default:
		// Successful completion: copy outputs back before publishing terminal status.
		e.completeSucceeded(ctx, msg, sess.ID, scratchDir, result, lp, &failed)
	}
}

// endedByOnRunGuard reports whether the assignment's OnRun action ended the
// task before anything was staged: no action is treated as succeeded, an empty
// command fails the task and sets *failed.
func (e *Executor) endedByOnRunGuard(ctx context.Context, msg *protocol.AssignMsg, sessID string, failed *bool) bool {
	if msg.OnRun == nil {
		e.logger.WarnContext(
			ctx, "executor: assignment has no OnRun action — treating as succeeded",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
		)
		zero := 0
		e.publishEnded(msg, sessID, "succeeded", &zero, "")
		return true
	}

	if msg.OnRun.Command == "" {
		e.logger.ErrorContext(
			ctx, "executor: OnRun.Command is empty — failing task",
			slog.String("task_id", msg.TaskID),
		)
		e.failPreExec(msg, sessID, failed, "task has an empty command")
		return true
	}
	return false
}

// stoppedBeforeStart reports whether a cancel or the shutdown drain stopped the
// run after its environments were entered and before its task started. If so,
// it publishes the outcome the canceled branch of runTask would, without
// staging or starting anything, and sets *failed.
func (e *Executor) stoppedBeforeStart(
	ctx context.Context, msg *protocol.AssignMsg, sessID string, taskCtx context.Context, failed *bool,
) bool {
	if taskCtx.Err() == nil {
		return false
	}
	*failed = true
	status, reason := e.stopOutcome()
	e.logger.InfoContext(
		ctx, "executor: task stopped before it started",
		slog.String("task_id", msg.TaskID),
		slog.String("attempt_id", msg.AttemptID),
		slog.String("session_id", sessID),
		slog.String("status", status),
	)
	e.publishEnded(msg, sessID, status, nil, reason)
	return true
}

// installLogProgress compiles msg.LogProgress against the task's frames and
// installs it on the output handler, when the assignment carries a block and the
// handler is a [ProgressPatternHook]. Progress is advisory, so a compile failure
// (impossible for a validated template, short of a missing frame parameter)
// disables it for this attempt with a warning and the task runs on.
func (e *Executor) installLogProgress(ctx context.Context, msg *protocol.AssignMsg) {
	if msg.LogProgress == nil {
		return
	}
	hook, ok := e.outputHandler.(ProgressPatternHook)
	if !ok {
		return
	}
	lp := msg.LogProgress
	m, err := logprogress.Compile(lp.Progress, lp.Status, msg.Parameters[lp.FrameParameter], protocol.SanitizeStatusText)
	if err != nil {
		e.logger.WarnContext(
			ctx, "executor: SQI_LOG_PROGRESS disabled for this attempt",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("frame_parameter", lp.FrameParameter),
			slog.Any("error", err),
		)
		return
	}
	hook.SetLogProgress(msg.AttemptID, m)
}

// completeSucceeded stages a successful task's outputs out and publishes its
// terminal "succeeded" status, or the "failed" one stageOutputs publishes when
// staging out fails.
func (e *Executor) completeSucceeded(ctx context.Context, msg *protocol.AssignMsg, sessID, scratchDir string, result processResult, lp *int, failed *bool) {
	if !e.stageOutputs(ctx, msg, sessID, scratchDir, failed) {
		e.m.ExecDuration.WithLabelValues("failed").Observe(result.duration().Seconds())
		return
	}
	zero := 0
	e.logger.InfoContext(
		ctx, "executor: task succeeded",
		slog.String("task_id", msg.TaskID),
		slog.String("attempt_id", msg.AttemptID),
		slog.String("session_id", sessID),
		slog.Int("pid", result.PID),
		slog.Duration("duration", result.duration()),
	)
	e.publishTerminal(msg, sessID, "succeeded", &zero, "", lp, result)
}

// failLogChecks publishes the terminal "failed" status for a task whose process
// exited 0 but whose output failed its SQI_LOG_CHECKS. The reason reaches the
// task's failure_reason (through the status message) and the worker's
// diagnostic log; it is not written to the task log.
func (e *Executor) failLogChecks(ctx context.Context, msg *protocol.AssignMsg, sessID string, result processResult, reason string, lp *int) {
	exitCode := result.ExitCode
	e.logger.InfoContext(
		ctx, "executor: task failed (SQI_LOG_CHECKS)",
		slog.String("task_id", msg.TaskID),
		slog.String("attempt_id", msg.AttemptID),
		slog.String("session_id", sessID),
		slog.String("reason", reason),
		slog.Int("pid", result.PID),
		slog.Duration("duration", result.duration()),
	)
	e.publishTerminal(msg, sessID, "failed", &exitCode, reason, lp, result)
}

// publishTerminal publishes a task's terminal status and records it in the
// task metrics under the same status label.
func (e *Executor) publishTerminal(msg *protocol.AssignMsg, sessID, status string, exitCode *int, reason string, lp *int, result processResult) {
	e.statusPub.Terminal(msg, sessID, status, exitCode, reason, lp, result.EndedAt)
	e.m.TasksTotal.WithLabelValues(status).Inc()
	e.m.ExecDuration.WithLabelValues(status).Observe(result.duration().Seconds())
}

// resolveAssignment resolves the OpenJD {{...}} format strings for a task:
//   - the OnRun action against the task-action scope (Task.Param.*, Task.File.*,
//     Param.*, Session.*);
//   - each step embedded file's data against that same task scope, so file data
//     may reference Task.Param.* and Task.File.* (the materialized path of any
//     step file, including itself or a sibling);
//
// The environment-variable map is the static environment session.Create
// resolved at enter time, each environment's values against its own
// environment scope (Task.Param.* is unavailable; Env.File.* is exposed so a
// value may reference that environment's file paths).
//
// It returns the resolved action copy, the environment-variable map, and the
// step embedded files with their data resolved, or a descriptive error naming
// the offending reference.
//
// msg.EXPR selects the resolution path. A base-spec assignment (EXPR false,
// the zero value) takes the fmtstring.Resolve-backed resolveAssignmentBaseSpec.
// An EXPR assignment takes resolveAssignmentExpr, which builds the worker-side
// (phase-3) symbol table with fmtres.TaskSymbols and resolves through the
// EXPR-aware evaluator (fmtres.ResolveActionExpr / ResolveEmbeddedFilesExpr).
func resolveAssignment(msg *protocol.AssignMsg, sess *session.Session) (*protocol.Action, map[string]string, []protocol.EmbeddedFile, error) {
	if !msg.EXPR {
		return resolveAssignmentBaseSpec(msg, sess)
	}
	return resolveAssignmentExpr(msg, sess)
}

// resolveAssignmentBaseSpec is resolveAssignment's base-spec implementation:
// plain "{{...}}" substitution via fmtstring.Resolve (through
// fmtres.TaskScope/ResolveAction/ResolveEmbeddedFiles). Every base-spec
// assignment resolves through this path, so its output must not depend on
// EXPR support.
func resolveAssignmentBaseSpec(msg *protocol.AssignMsg, sess *session.Session) (*protocol.Action, map[string]string, []protocol.EmbeddedFile, error) {
	workDir := sess.WorkDir
	pathMapFile := sess.PathMappingRulesFile()
	hasPathMap := sess.HasPathMappingRules()

	taskScope := fmtres.TaskScope(msg.JobParameters, msg.Parameters, workDir, pathMapFile, hasPathMap)
	// Expose Task.File.<name> -> materialized path for each step embedded file
	// before resolving the command/args and the files' own data, so both can
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

	// Reuse the static environment that session.Create resolved at enter
	// time. Each environment's Variables are resolved against that
	// environment's own Env.File.* scope; re-resolving here against a merged
	// all-environments scope would repeat the work and could resolve a
	// same-named Env.File.* reference to a different file (last-wins across
	// environments).
	resolvedEnvVars := sess.StaticEnv()
	return resolvedRun, resolvedEnvVars, resolvedFiles, nil
}

// resolveAssignmentExpr is resolveAssignment's EXPR-aware implementation. It
// builds the worker-side (phase-3) symbol table (fmtres.TaskSymbols),
// evaluates the task's let: bindings into it once (fmtres.ApplyTaskLet: a
// second call over the same table makes every binding fail the shadow
// check), and resolves OnRun and the step embedded files against that one
// table via ResolveActionExpr/ResolveEmbeddedFilesExpr.
func resolveAssignmentExpr(msg *protocol.AssignMsg, sess *session.Session) (*protocol.Action, map[string]string, []protocol.EmbeddedFile, error) {
	workDir := sess.WorkDir
	pathMapFile := sess.PathMappingRulesFile()
	hasPathMap := sess.HasPathMappingRules()

	// sess.ExprBudget() is the one fmtres.AssignmentBudget the whole
	// assignment shares: every environment table the session entered
	// (session.go's enterOne) charges it, and so does this task's table, so
	// the assignment's total resolved-position and retained-byte spend across
	// every table it builds is bounded together. See fmtres.AssignmentBudget.
	budget := sess.ExprBudget()

	// The error label is wider than the base-spec path's "step embedded
	// files" because TaskSymbols also fails on job- and task-parameter
	// binding (mapPathParamValue, via bindJobParamSymbols /
	// bindTaskParamSymbols), such as a PATH parameter that cannot be mapped.
	//
	// budget supplies the limits here and is not charged: binding a PATH
	// parameter runs an apply_path_mapping evaluation, which must be metered
	// by the same operator-configured limits as every evaluation below. The
	// budget must therefore be obtained before the table is built, or that
	// evaluation falls back to the compiled-in defaults.
	syms, err := fmtres.TaskSymbols(msg, workDir, pathMapFile, hasPathMap, budget)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build expression symbols: %w", err)
	}

	// One call over this table (see fmtres.ApplyTaskLet). Every resolution
	// below (OnRun, then the embedded files) reuses this syms.
	if err := fmtres.ApplyTaskLet(msg, syms, msg.PathMap, budget); err != nil {
		return nil, nil, nil, fmt.Errorf("let bindings: %w", err)
	}
	resolvedRun, err := fmtres.ResolveActionExpr(msg.OnRun, syms, msg.PathMap, budget)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve command: %w", err)
	}
	resolvedFiles, err := fmtres.ResolveEmbeddedFilesExpr(msg.EmbeddedFiles, syms, msg.PathMap, budget)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve embedded file data: %w", err)
	}

	// Reuse the static environment that session.Create resolved at enter
	// time, for the reason given in resolveAssignmentBaseSpec.
	resolvedEnvVars := sess.StaticEnv()
	return resolvedRun, resolvedEnvVars, resolvedFiles, nil
}

// preExecExitCode returns the exit code reported for a task that failed before
// its process ran.
func preExecExitCode() *int {
	code := -1
	return &code
}

// failPreExec publishes the terminal "failed" status for a pre-execution
// failure (exit code -1) and marks the task failed. Callers log their own
// context-specific message before calling.
func (e *Executor) failPreExec(msg *protocol.AssignMsg, sessID string, failed *bool, reason string) {
	*failed = true
	e.publishEnded(msg, sessID, "failed", preExecExitCode(), reason)
}

// publishSetupFailure publishes the outcome of a run whose session could not be
// created. A run whose context was stopped gets [Executor.stopOutcome];
// otherwise it failed with exit code -1 and err's text. No session exists, so
// the messages carry no session ID.
func (e *Executor) publishSetupFailure(msg *protocol.AssignMsg, runCtx context.Context, err error) {
	if runCtx.Err() == nil {
		e.publishEnded(msg, "", "failed", preExecExitCode(), err.Error())
		return
	}
	status, reason := e.stopOutcome()
	e.publishEnded(msg, "", status, nil, reason)
}

// stopOutcome returns the terminal status and reason for a run whose context
// was canceled: "failed" with [protocol.MessageWorkerShutdown] when the
// shutdown drain stopped it, "canceled" with no reason when a cancel did.
func (e *Executor) stopOutcome() (status, reason string) {
	if e.shuttingDown.Load() {
		return "failed", protocol.MessageWorkerShutdown
	}
	return "canceled", ""
}

// publishEnded publishes status for a task that ended without a process
// result, and records it in the task metrics under status. A nil exitCode
// reports none.
//
// No "running" is sent first: the server moves an assigned task straight to
// its terminal status (store.ValidateTaskTransition), and the publisher drops
// a still-pending first "running" when the terminal is handed over. An inline
// "running" would cost the caller (the lease loop, for a refused or failed
// dispatch) a second ack timeout while JetStream is down. Terminal waits at
// most one and leaves a report the stream has not acknowledged to retry in the
// background.
func (e *Executor) publishEnded(msg *protocol.AssignMsg, sessID, status string, exitCode *int, reason string) {
	e.statusPub.Terminal(msg, sessID, status, exitCode, reason, nil, time.Now())
	e.m.TasksTotal.WithLabelValues(status).Inc()
}

// failResolution publishes the terminal "failed" status for a task that could
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
// has exited and its output has been read as procout.Output.Wait describes.
//
// lookup contains the resolved-mode path map parsed by the caller; it is
// applied to the OnRun command and args before the process is started.
//
// Callers must check [processResult.Err] for start/wait failures and
// [processResult.TimedOut] / [processResult.Canceled] for forced termination.
// resolvedRun is the OnRun action with its format strings already resolved by
// the caller; envVars is the merged, format-string-resolved environment-variable
// map to apply on top of the inherited process environment; handler receives
// every scrubbed output line.
func (e *Executor) execProcess(ctx context.Context, msg *protocol.AssignMsg, sess *session.Session, run *taskRun, lookup *pathmap.Lookup, resolvedRun *protocol.Action, envVars map[string]string, handler OutputHandler) processResult {
	// Apply path-delivery transformations: swap_in_place substitutes paths in
	// the action, command_flags appends rendered flag strings, and environment
	// sets the configured variable.  translation_file is written at session
	// creation (and rewritten by buildEffectiveLookup when staging adds rules),
	// not here.
	action, envVars := applyDeliveries(msg.PathDeliveries, lookup, resolvedRun, envVars)

	// Swap the job's command name for the operator's worker.commands entry, if
	// there is one. After applyDeliveries so a path substituted into the command
	// is mapped like any other; before exec.Command so the mapped path is what
	// the process is started from. The environment onEnter/onExit launch site in
	// the session package makes the same call.
	action = sess.ResolveAction(action)

	// exec.Command, not exec.CommandContext (noctx): CommandContext sends a
	// fixed signal when ctx is canceled, bypassing the OpenJD
	// Action.cancelation policy (TERMINATE vs NOTIFY_THEN_TERMINATE) that
	// killAndWait applies.
	cmd := exec.Command(action.Command, action.Args...) //nolint:gosec,noctx // command from server-signed assignment; context handled manually
	cmd.Dir = sess.WorkDir

	// Start the task in its own process group so a timeout or cancel
	// terminates the task and every child it spawns, and so procout keeps
	// waiting for output while any process of the group runs. On Windows the
	// group is the job object proctree.Supervise creates after Start.
	proctree.Configure(cmd)

	// The task's OnRun action is job-supplied code, and this is one of the two
	// launch sites that must carry a credential (the other is the OpenJD
	// environment onEnter/onExit action in session.go's runAction). It is
	// applied after proctree.Configure because isolation.Apply keeps the
	// SysProcAttr fields already set, such as Setpgid, and would not see ones
	// set later.
	if err := applyCredential(cmd, sess.Credential()); err != nil {
		return earlyResult(fmt.Errorf("apply run-as-user credential: %w", err), time.Time{})
	}

	// Merge the session's dynamic environment (accumulated from openjd_env /
	// openjd_redacted_env / openjd_unset_env directives emitted by environment
	// actions) on top of the task's static environment variables. Precedence:
	// session base env (lowest, already filtered once at session creation) <
	// static env Variables < session dynamic env; openjd_unset_env removes a
	// variable even if a static entry or the session base environment set it.
	overrides := envVars
	if dyn := sess.EnvOverrides(); len(dyn) > 0 {
		overrides = make(map[string]string, len(envVars)+len(dyn))
		maps.Copy(overrides, envVars)
		maps.Copy(overrides, dyn)
	}
	cmd.Env = envutil.BuildFromBase(sess.BaseEnv(), overrides, sess.EnvUnset())

	startedAt := time.Now()

	// Publish "running" status before Start so the server records the attempt
	// even if the command is not found or cannot be executed.  If Start fails
	// the caller publishes a terminal "failed" immediately after.
	// The status carries no progress: the process has emitted no output.
	e.statusPub.Running(msg, sess.ID, startedAt)

	// procout, not StdoutPipe, so the task's exit is seen while a descendant
	// still holds its output open, such as Unreal Editor's detached
	// UnrealTraceServer.
	out, err := procout.Start(cmd)
	if err != nil {
		return earlyResult(fmt.Errorf("start %q: %w", action.Command, err), startedAt)
	}

	// Place the process (and, on Windows, everything it spawns) under
	// supervision immediately after Start so killAndWait reaches the full
	// tree, including a grandchild orphaned before the kill, which
	// "taskkill /F /T" would miss. Release, deferred below, does not kill
	// processes still in the tree, matching POSIX; see internal/worker/proctree.
	tree, err := proctree.Supervise(cmd)
	if err != nil {
		// The process exists (on Windows still suspended, or already killed
		// by closing its job), but nothing has registered it for cleanup yet
		// (run.pid, sess.AddTask, the output readers) and there is no tree to
		// call Kill on. Kill and reap it here so a Supervise failure
		// (resource exhaustion, restrictive policy, or a run-as-user
		// permission problem) does not leave an unmanaged process behind.
		if killErr := out.Abort(); killErr != nil {
			e.logger.WarnContext(context.Background(), "executor: kill process after supervise failure", slog.Any("error", killErr))
		}
		return earlyResult(fmt.Errorf("supervise task process tree: %w", err), startedAt)
	}
	defer func() {
		if relErr := tree.Release(); relErr != nil {
			// context.Background(), not ctx: this defer can run after ctx is
			// canceled (task done, worker shutting down), and the warning
			// should still be logged, as in flushTaskLogs.
			e.logger.WarnContext(context.Background(), "executor: release process tree", slog.Any("error", relErr))
		}
	}()

	pid := cmd.Process.Pid
	run.pid = pid
	run.startedAt = startedAt

	// Track this task ID in the session so Session.ActiveTaskCount() is accurate.
	sess.AddTask(msg.TaskID)
	defer sess.RemoveTask(msg.TaskID)

	// Read stdout and stderr concurrently to prevent pipe-buffer
	// deadlocks (if one pipe fills up waiting for the other to be drained,
	// both sides block indefinitely).
	//
	// With SQI_LOG_CHECKS the caller wraps handler so the checker sees each line
	// after redaction (scanOutput scrubs before it calls the handler), the same
	// text the task log shows.
	var readWg sync.WaitGroup
	readWg.Add(2)
	go func() {
		defer readWg.Done()
		scanOutput(ctx, out.Stdout, "stdout", msg.TaskID, msg.AttemptID, sess.ID, handler, sess.ScrubRedacted, e.logger)
	}()
	go func() {
		defer readWg.Done()
		scanOutput(ctx, out.Stderr, "stderr", msg.TaskID, msg.AttemptID, sess.ID, handler, sess.ScrubRedacted, e.logger)
	}()

	// Wait for the process, then end the scanners and wait for them, so every
	// line read reaches the handler before the result is reported.
	waitDone := make(chan error, 1)
	go func() {
		stillOpen, waitErr := out.Wait(tree.Alive)
		readWg.Wait()
		if stillOpen {
			e.logger.WarnContext(ctx,
				"executor: the task and its process group exited but a process outside the group still holds the output; output after the drain delay is not captured",
				slog.String("task_id", msg.TaskID), slog.Duration("waited", procout.DrainDelay))
		}
		waitDone <- waitErr
	}()

	var taskTimeout time.Duration
	if action.TimeoutSeconds > 0 {
		taskTimeout = time.Duration(action.TimeoutSeconds) * time.Second
	}

	// The OpenJD Action.cancelation method governs how the process is terminated
	// on timeout or cancellation (TERMINATE = immediate SIGKILL, the spec default;
	// NOTIFY_THEN_TERMINATE = SIGTERM then SIGKILL after the notify period).
	if taskTimeout > 0 {
		return e.waitWithTimeout(ctx, tree, run, waitDone, startedAt, pid, taskTimeout, action.Cancelation)
	}
	return e.waitForCompletion(ctx, tree, run, waitDone, startedAt, pid, action.Cancelation)
}

// ── Wait helpers ──────────────────────────────────────────────────────────────

// waitForCompletion blocks until the process exits normally or ctx is canceled.
// cancelation is the OpenJD Action.cancelation method honored when the process
// must be forcibly terminated.
func (e *Executor) waitForCompletion(
	ctx context.Context,
	tree *proctree.Tree,
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
		return e.killAndWait(ctx, tree, run, waitDone, startedAt, pid, false, true, cancelation)
	}
}

// waitWithTimeout blocks until the process exits, the per-task timeout
// elapses, or ctx is canceled — whichever comes first.  cancelation is the
// OpenJD Action.cancelation method honored when the process must be forcibly
// terminated (on timeout or cancellation).
func (e *Executor) waitWithTimeout(
	ctx context.Context,
	tree *proctree.Tree,
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
		return e.killAndWait(ctx, tree, run, waitDone, startedAt, pid, true, false, cancelation)
	case <-ctx.Done():
		return e.killAndWait(ctx, tree, run, waitDone, startedAt, pid, false, true, cancelation)
	}
}

// defaultNotifyPeriod is the OpenJD default notify period for an OnRun action
// using NOTIFY_THEN_TERMINATE when notifyPeriodInSeconds is unset (other
// actions default to 30 s; see internal/worker/session).
const defaultNotifyPeriod = 120 * time.Second

// killAndWait terminates the process and blocks until waitDone reports that it
// has exited and its output readers have finished.
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
// Telling worker shutdown from a per-task cancel relies on DrainAndShutdown
// setting the shuttingDown flag before it cancels execCtx.  A concurrent
// external Cancel racing a shutdown is harmless: the server keeps the first
// terminal status for a task and ignores later ones.
func (e *Executor) killAndWait(
	ctx context.Context,
	tree *proctree.Tree,
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
		notify, gracePeriod = proctree.Resolve(cancelation, defaultNotifyPeriod)
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
		if killErr := tree.Kill(); killErr != nil {
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
	if err := tree.Notify(); err != nil {
		e.logger.WarnContext(
			ctx, "executor: SIGTERM failed — escalating to SIGKILL immediately",
			slog.String("task_id", run.taskID),
			slog.String("attempt_id", run.attemptID),
			slog.Int("pid", pid),
			slog.Any("error", err),
		)
		if killErr := tree.Kill(); killErr != nil {
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
	// time.NewTimer, not time.After, so the timer is stopped when the process
	// exits before the grace period elapses.
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
		if killErr := tree.Kill(); killErr != nil {
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

// earlyResult is the result of a run that failed with err before there was a
// process to wait for. It ends now; startedAt is when the run started, or
// zero when it failed before that, in which case it starts now too.
func earlyResult(err error, startedAt time.Time) processResult {
	endedAt := time.Now()
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	return processResult{Err: err, StartedAt: startedAt, EndedAt: endedAt}
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

// takeFailReason retrieves the openjd_fail reason stored for attemptID by the
// output handler's TaskLifecycleHook, if wired. Returns ("", false) when no
// openjd_fail was seen or no hook is configured.
func (e *Executor) takeFailReason(attemptID string) (string, bool) {
	if hook, ok := e.outputHandler.(TaskLifecycleHook); ok {
		return hook.TakeFailReason(attemptID)
	}
	return "", false
}

// ── Path-delivery helpers ─────────────────────────────────────────────────────

// applyDeliveries runs the non-staging deliveries over the effective lookup:
// swap_in_place substitutes paths in the action; command_flags appends rendered
// flags; environment sets the configured variable. translation_file is handled
// at session/executor file-write time, not here. Returns the (possibly new)
// action and the env map; a non-nil env is written in place, and a nil one is
// replaced by a new map when the environment delivery sets a variable.
//
// Deliveries are applied in a fixed order (swap_in_place → command_flags →
// environment) whatever order the template declares them in, so a template that
// lists command_flags before swap_in_place does not have its appended flag
// strings substituted a second time.
func applyDeliveries(deliveries []protocol.PathDelivery, lookup *pathmap.Lookup, action *protocol.Action, env map[string]string) (outAction *protocol.Action, outEnv map[string]string) {
	// Index enabled deliveries by kind so we can apply in fixed canonical order.
	type kindCfg struct{ pattern, variable string }
	kinds := make(map[string]kindCfg, len(deliveries))
	for _, d := range deliveries {
		kinds[d.Kind] = kindCfg{d.Pattern, d.Variable}
	}

	out := action
	// 1. swap_in_place: substitute source paths in command and args first so that
	//    the appended command_flags use the original (untranslated) source path.
	if _, ok := kinds["swap_in_place"]; ok {
		out = lookup.ApplyToAction(out)
	}
	// 2. command_flags: append rendered flag strings after swap so they are not
	//    re-substituted by any subsequent (mis-ordered) swap.
	if c, ok := kinds["command_flags"]; ok {
		flags := lookup.CommandFlags(c.pattern)
		if len(flags) > 0 {
			args := make([]string, 0, len(out.Args)+len(flags))
			args = append(args, out.Args...)
			args = append(args, flags...)
			na := *out
			na.Args = args
			out = &na
		}
	}
	// 3. environment: set the configured variable. The assignment may carry no
	//    OpenJD environment variables, in which case env is nil — allocate it
	//    before writing rather than panicking on a nil-map assignment.
	if c, ok := kinds["environment"]; ok {
		if v := lookup.EnvValue(); v != "" {
			if env == nil {
				env = make(map[string]string, 1)
			}
			env[c.variable] = v
		}
	}
	return out, env
}

// hasDelivery reports whether deliveries contains an entry with the given kind.
// It is a local copy because scheduler and executor are same-level internal
// packages that must not cross-import.
func hasDelivery(ds []protocol.PathDelivery, kind string) bool {
	for _, d := range ds {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

// preExecValidate runs the pre-exec steps that sit between resolveAssignment
// and the process launch: (1) compiling the step's SQI_LOG_CHECKS, (2) the s3://
// path guard and (3) step embedded-file materialization. It returns the log
// checker (nil when the step has no checks) and true when all steps pass; false
// when any failed the task (caller must return).
func (e *Executor) preExecValidate(
	ctx context.Context,
	msg *protocol.AssignMsg,
	sess *session.Session,
	resolvedRun *protocol.Action,
	resolvedEnvVars map[string]string,
	resolvedFiles []protocol.EmbeddedFile,
	failed *bool,
) (*logChecker, bool) {
	// The server validated the patterns at submission, so a compile error is not
	// expected here; it fails the task before anything runs.
	checker, err := newLogChecker(msg.LogChecks)
	if err != nil {
		e.logger.ErrorContext(
			ctx, "executor: invalid SQI_LOG_CHECKS pattern — failing task",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.Any("error", err),
		)
		e.failPreExec(msg, sess.ID, failed, "invalid SQI_LOG_CHECKS pattern: "+err.Error())
		return nil, false
	}
	if e.guardUnstagedS3(ctx, msg, sess.ID, resolvedRun, resolvedEnvVars, failed) {
		return nil, false
	}
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
		e.failPreExec(msg, sess.ID, failed, err.Error())
		return nil, false
	}
	return checker, true
}

// guardUnstagedS3 checks whether the resolved command/args/env contains an
// s3:// URI without stage_locally enabled, and if so fails the task pre-exec
// with a clear reason. It returns true when the task was failed (caller must
// return), false otherwise.
func (e *Executor) guardUnstagedS3(
	ctx context.Context,
	msg *protocol.AssignMsg,
	sessID string,
	resolvedRun *protocol.Action,
	resolvedEnvVars map[string]string,
	failed *bool,
) bool {
	if hasDelivery(msg.PathDeliveries, "stage_locally") {
		return false
	}
	p, ok := FirstS3Path(resolvedRun, resolvedEnvVars)
	if !ok {
		return false
	}
	e.logger.ErrorContext(
		ctx, "executor: object-store path without staging — failing task",
		slog.String("task_id", msg.TaskID),
		slog.String("attempt_id", msg.AttemptID),
		slog.String("path", p),
	)
	e.failPreExec(msg, sessID, failed, fmt.Sprintf(
		"resolved path %q is an object-store URI but stage_locally is not enabled for this job; "+
			"enable the SQI_PATH_TRANSLATION stage_locally delivery or use a filesystem/mounted root", p,
	))
	return true
}

// FirstS3Path returns the first s3:// URI found among the resolved action's
// command, args, and env-var values, or ("", false) if none. It underpins the
// pre-exec guard that rejects an object-store path which was not staged to local
// scratch. Env iteration order is unspecified, so tests should place the token in
// the command or args when they assert a specific value.
func FirstS3Path(action *protocol.Action, env map[string]string) (string, bool) {
	if action != nil {
		if s, ok := s3Token(action.Command); ok {
			return s, true
		}
		for _, a := range action.Args {
			if s, ok := s3Token(a); ok {
				return s, true
			}
		}
	}
	for _, v := range env {
		if s, ok := s3Token(v); ok {
			return s, true
		}
	}
	return "", false
}

// s3Token returns the substring of s starting at the first lowercase "s3://"
// occurrence, or ("", false) if absent.
func s3Token(s string) (string, bool) {
	if i := strings.Index(s, "s3://"); i >= 0 {
		return s[i:], true
	}
	return "", false
}

// buildEffectiveLookup handles the staging + path-map phases of runTask.
// It stages inputs if the stage_locally delivery is enabled, builds the
// combined (staging-rules-prepended) lookup so staging paths take precedence,
// and rewrites path_mapping.json when staging rules are added alongside
// translation_file.
//
// On any failure it calls failPreExec; if staging was not yet active it returns
// (nil, "", false), otherwise it returns (nil, scratchDir, false) so the caller's
// deferred Cleanup always receives the real scratch path. On success it returns
// the effective lookup, the scratch directory (empty string if staging was not
// active), and true.
func (e *Executor) buildEffectiveLookup(
	ctx context.Context,
	msg *protocol.AssignMsg,
	sessID, workDir string,
	cred *isolation.Credential,
	failed *bool,
) (*pathmap.Lookup, string, bool) {
	var scratchDir string
	var stagingRules []protocol.PathMapRule

	if hasDelivery(msg.PathDeliveries, "stage_locally") {
		if !e.stager.Configured() {
			e.failPreExec(msg, sessID, failed, "worker not configured for staging (set staging.scratch_dir and staging.sync_command, or enable staging.defaults)")
			return nil, "", false
		}
		var stErr error
		stagingRules, scratchDir, stErr = e.stager.StageIn(ctx, msg.JobID, msg.AttemptID, msg.Staging, cred)
		if stErr != nil {
			e.failPreExec(msg, sessID, failed, stErr.Error())
			return nil, "", false
		}
	}

	effectiveRules := append(append([]protocol.PathMapRule{}, stagingRules...), msg.PathMap...)
	lookup, err := pathmap.NewLookup(effectiveRules)
	if err != nil {
		e.failPreExec(msg, sessID, failed, err.Error())
		return nil, scratchDir, false
	}

	if len(stagingRules) > 0 && hasDelivery(msg.PathDeliveries, "translation_file") {
		if werr := pathmap.WritePathMappingFile(workDir, effectiveRules); werr != nil {
			e.failPreExec(msg, sessID, failed, werr.Error())
			return nil, scratchDir, false
		}
	}

	return lookup, scratchDir, true
}

// stageOutputs copies staged outputs back to their original paths after a
// successful process exit. If staging was not active (scratchDir == "") it
// returns true immediately. On copy-out failure it logs the error, publishes a
// terminal failed status, sets *failed, and returns false; the caller must
// return.
func (e *Executor) stageOutputs(ctx context.Context, msg *protocol.AssignMsg, sessID, scratchDir string, failed *bool) bool {
	if scratchDir == "" {
		return true
	}
	soErr := e.stager.StageOut(ctx, scratchDir, msg.Staging)
	if soErr == nil {
		return true
	}
	e.logger.ErrorContext(
		ctx, "executor: stage-out failed",
		slog.String("task_id", msg.TaskID),
		slog.Any("error", soErr),
	)
	*failed = true
	minusOne := -1
	e.statusPub.Terminal(msg, sessID, "failed", &minusOne, soErr.Error(), nil, time.Now())
	e.m.TasksTotal.WithLabelValues("failed").Inc()
	return false
}

// ── Output capture ────────────────────────────────────────────────────────────

// scanOutput reads lines from r, scrubs each through scrub, and forwards the
// result to handler.HandleLine. It returns when r reaches EOF or a read fails.
// For a procout stream, EOF comes once every process holding the output has
// closed it, or when procout.Output.Wait ends delivery after the drain delay.
// No line is too long to read, so what a process prints cannot stop the
// reading and leave it blocked on a full pipe.
//
// A line longer than maxOutputLine (64 KiB) is forwarded as consecutive pieces
// of at most that many bytes, never cut inside a UTF-8 character, and the first
// split on a stream logs one WARN. Each piece is scrubbed and handled on its
// own, as if it were a line: a redacted value that straddles a piece boundary
// is not guaranteed to be scrubbed (one longer than 64 KiB never is), an openjd
// directive longer than 64 KiB reaches the handler as its first piece only, and
// a later piece that happens to begin with a directive prefix is taken for a
// directive.
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
	lines := &lineSplitter{
		emit: func(b []byte) {
			line := string(b)
			if scrub != nil {
				line = scrub(line)
			}
			handler.HandleLine(ctx, taskID, attemptID, sessionID, stream, line)
		},
		onSplit: sync.OnceFunc(func() {
			logger.WarnContext(
				context.Background(), "executor: task output line over 64 KiB delivered in pieces",
				slog.String("task_id", taskID),
				slog.String("attempt_id", attemptID),
				slog.String("stream", stream),
			)
		}),
	}
	br := bufio.NewReaderSize(r, maxOutputLine)
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			lines.end(chunk[:len(chunk)-1])
			continue
		case errors.Is(err, bufio.ErrBufferFull):
			lines.partial(chunk)
			continue
		}
		// EOF or a read error: deliver the final unterminated line, if any.
		if len(chunk) > 0 || lines.split {
			lines.end(chunk)
		}
		// A non-EOF error here typically means the process was killed and the
		// pipe was closed abruptly; log at debug level to avoid alarming
		// operators on expected termination paths.
		if !errors.Is(err, io.EOF) {
			// Use a background context in case ctx is already canceled.
			logger.DebugContext(
				context.Background(), "executor: output scan error",
				slog.String("task_id", taskID),
				slog.String("attempt_id", attemptID),
				slog.String("stream", stream),
				slog.Any("error", err),
			)
		}
		return
	}
}
