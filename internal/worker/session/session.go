// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session manages the lifecycle of OpenJD sessions on the worker.
//
// A Session is the ephemeral execution context within which one or more
// tasks from the same job step run. It provides:
//   - An isolated working directory under <data_dir>/sessions/<session_id>/
//   - Ordered environment entry (OnEnter actions run in declaration order)
//   - Ordered environment exit (OnExit actions run in reverse order)
//   - Safe concurrent task tracking via AddTask / RemoveTask
//
// # Session identity
//
// Session IDs are worker-generated UUIDs. The ID is included in every
// [protocol.TaskStatusMsg] and [protocol.LogChunkMsg] published from within
// the session so the server can group task attempts by session for debugging
// (consistent with the session_id column on task_attempts; see
// docs/architecture.md, "SQLite schema overview").
//
// # Phase 1 scope
//
// In Phase 1, sessions are not stored as database rows — they are worker-side
// runtime constructs. The server records the session ID only on task_attempts.
// A dedicated sessions table (for session-reuse scheduling) is deferred to
// Phase 2; see docs/architecture.md ("SQLite schema overview").
//
// # Usage
//
//	mgr := session.NewManager(cfg.Worker.DataDir, cfg.Worker.KeepFailedSessions, logger)
//
//	s, err := mgr.Create(ctx, assignMsg)
//	if err != nil { ... }
//	defer mgr.Cleanup(ctx, s, true) // true = "failed" path; use false on success
//
//	// Execute tasks within s.WorkDir, using s.ID as the SessionID in status messages.
//	s.AddTask(taskID)
//	// ... run task process ...
//	s.RemoveTask(taskID)
//
//	// On session end:
//	if err := s.ExitEnvironments(ctx, logger); err != nil { ... }
//	mgr.Cleanup(ctx, s, failed)
package session

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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Session ───────────────────────────────────────────────────────────────────

// Session is the ephemeral execution context for one or more tasks from the
// same job step. Obtain one via [Manager.Create]; end it by calling
// [Session.ExitEnvironments] followed by [Manager.Cleanup].
//
// Session is safe for concurrent use. Multiple tasks may execute within a
// single session simultaneously (subject to max_concurrent_tasks).
type Session struct {
	// ID is the worker-generated unique identifier for this session.
	// Callers MUST include this in every [protocol.TaskStatusMsg] (as
	// SessionID) and every [protocol.LogChunkMsg] published from within
	// the session so the server can group attempts by session.
	ID string

	// WorkDir is the absolute path to the session's isolated working directory,
	// created by [Manager.Create] and removed by [Manager.Cleanup].
	// Commands launched within the session run with this as their cwd so that
	// relative file references resolve correctly.
	WorkDir string

	// JobID is the ID of the job that owns this session.
	JobID string

	// CreatedAt is the wall-clock time the session was created.
	CreatedAt time.Time

	// jobParams are the job-level parameter values for the assignment, used to
	// build the environment-action format-string scope (Param.<n> / RawParam.<n>)
	// when resolving environment onEnter/onExit actions and variable values.
	jobParams map[string]string

	// pathMapFile is the absolute path to the OpenJD path-mapping file written
	// into WorkDir at session creation, exposed to format strings as
	// Session.PathMappingRulesFile. Empty when the assignment carried no rules.
	pathMapFile string
	// hasPathMap reports whether the assignment carried any path-mapping rules,
	// exposed to format strings as Session.HasPathMappingRules.
	hasPathMap bool

	// staticEnv is the merged, fully-resolved static environment from every
	// environment's Variables, accumulated in enterOne. Each environment's
	// values are resolved against that environment's OWN Env.File.* scope (the
	// authoritative resolution), and later environments override earlier ones.
	// The executor reuses this for the task's static environment rather than
	// re-resolving msg env vars against a merged all-environments scope, which
	// could resolve a same-named Env.File reference differently. Built during
	// Create before the session is shared; read-only afterward.
	staticEnv map[string]string

	mu          sync.Mutex
	activeTasks []string                     // task IDs currently executing; managed via AddTask / RemoveTask
	enteredEnvs []protocol.AssignEnvironment // environments entered in declaration order; nil after ExitEnvironments

	// dynamicEnv accumulates environment variables set via openjd_env /
	// openjd_redacted_env directives emitted to stdout by environment actions.
	// These apply to all SUBSEQUENT actions in the session (later environments'
	// onEnter/onExit actions and the task OnRun). Guarded by mu: written from the
	// stdout-reading goroutine in runAction and read concurrently by executing
	// tasks via EnvOverrides.
	dynamicEnv map[string]string
	// dynamicUnset records variables removed via openjd_unset_env. A key here
	// must be absent from subsequent actions even if a static Variables entry or
	// the worker environment sets it. Guarded by mu.
	dynamicUnset map[string]bool
	// redactedValues holds the cleartext values supplied via openjd_redacted_env
	// so they can be scrubbed from any logged output. Guarded by mu.
	redactedValues map[string]bool
}

// PathMappingRulesFile returns the absolute path to the OpenJD path-mapping
// file written into the session working directory, or "" when the assignment
// carried no path-mapping rules. Exposed to format strings as
// Session.PathMappingRulesFile.
func (s *Session) PathMappingRulesFile() string { return s.pathMapFile }

// HasPathMappingRules reports whether the assignment carried any path-mapping
// rules. Exposed to format strings as Session.HasPathMappingRules.
func (s *Session) HasPathMappingRules() bool { return s.hasPathMap }

// WriteEmbeddedFiles materializes files into the session working directory.
// It is called by the executor before running the step OnRun action so that
// step-level embedded files are available to the task command.
//
// Each file's Runnable flag is honored (execute permission set) and its
// EndOfLine value is applied before writing. Returns an error naming the
// first file that could not be written, which the caller should use to fail
// the task via the established pre-execution failure path.
func (s *Session) WriteEmbeddedFiles(files []protocol.EmbeddedFile) error {
	return writeEmbeddedFiles(s.WorkDir, files)
}

// ActiveTaskCount returns the number of tasks currently executing within this
// session. Safe for concurrent use.
func (s *Session) ActiveTaskCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.activeTasks)
}

// AddTask records taskID as executing within this session.
// Called by the executor when a task process is launched.
func (s *Session) AddTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeTasks = append(s.activeTasks, taskID)
}

// RemoveTask removes taskID from the active task list.
// Called by the executor when a task process exits.
// If taskID is not present the call is a no-op.
func (s *Session) RemoveTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.activeTasks {
		if id != taskID {
			s.activeTasks[n] = id
			n++
		}
	}
	s.activeTasks = s.activeTasks[:n]
}

// ActiveTaskIDs returns a snapshot of the task IDs currently executing within
// this session. Safe for concurrent use.
func (s *Session) ActiveTaskIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, len(s.activeTasks))
	copy(ids, s.activeTasks)
	return ids
}

// ExitEnvironments runs each entered environment's OnExit action in reverse
// entry order, ensuring host state is restored predictably regardless of
// session outcome.
//
// All teardown actions are attempted even if an earlier one fails. Errors
// from individual OnExit actions are logged as warnings, collected, and the
// first error encountered is returned.
//
// ExitEnvironments is idempotent: after it is called the first time the
// entered-environments list is cleared, so subsequent calls are no-ops.
// [Manager.Cleanup] always calls ExitEnvironments, so callers that invoke it
// explicitly beforehand (e.g., on the cancellation path) do not need to skip
// the Cleanup call — the second invocation will be a safe no-op.
func (s *Session) ExitEnvironments(ctx context.Context, logger *slog.Logger) error {
	s.mu.Lock()
	envs := make([]protocol.AssignEnvironment, len(s.enteredEnvs))
	copy(envs, s.enteredEnvs)
	s.enteredEnvs = nil // clear so a second call is a safe no-op
	s.mu.Unlock()

	var firstErr error
	for _, v := range slices.Backward(envs) {
		env := v
		if env.OnExit == nil {
			continue
		}
		// Resolve {{...}} format strings in the onExit action and the environment
		// variable values against the environment-action scope.  Continue teardown
		// of the remaining environments even if one fails to resolve.
		resolvedExit, resolvedVars, err := s.resolveEnvAction(env.OnExit, env.Variables, env.EmbeddedFiles)
		if err != nil {
			logger.WarnContext(
				ctx, "session: environment exit action could not be resolved — continuing teardown",
				slog.String("session_id", s.ID),
				slog.String("env", env.Name),
				slog.Any("error", err),
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("environment %q exit: resolve: %w", env.Name, err)
			}
			continue
		}
		logger.InfoContext(
			ctx, "session: exiting environment",
			slog.String("session_id", s.ID),
			slog.String("env", env.Name),
		)
		envVars, unset := s.actionEnv(resolvedVars)
		if err := runAction(ctx, resolvedExit, s.WorkDir, envVars, unset, logger, s.actionLineHandler(ctx, logger)); err != nil {
			logger.WarnContext(
				ctx, "session: environment exit action failed — continuing teardown",
				slog.String("session_id", s.ID),
				slog.String("env", env.Name),
				slog.Any("error", err),
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("environment %q exit: %w", env.Name, err)
			}
		} else {
			logger.InfoContext(
				ctx, "session: environment exited",
				slog.String("session_id", s.ID),
				slog.String("env", env.Name),
			)
		}
	}
	return firstErr
}

// ── Manager ───────────────────────────────────────────────────────────────────

// Manager creates and cleans up sessions. A single Manager is constructed
// once at worker startup and shared by the executor for the process lifetime.
type Manager struct {
	dataDir            string
	keepFailedSessions bool
	logger             *slog.Logger
}

// NewManager returns a Manager that stores session working directories under
// <dataDir>/sessions/. keepFailedSessions controls whether working directories
// for failed sessions are retained for post-mortem inspection.
func NewManager(dataDir string, keepFailedSessions bool, logger *slog.Logger) *Manager {
	return &Manager{
		dataDir:            dataDir,
		keepFailedSessions: keepFailedSessions,
		logger:             logger,
	}
}

// Create allocates a new Session for the given assignment: generates a UUID
// as the session ID, creates the working directory under
// <data_dir>/sessions/<session_id>/, and enters each environment in
// declaration order.
//
// If any OnEnter action fails, already-entered environments are exited in
// reverse order and the working directory is removed before the
// error is returned. Create always returns either a ready-to-use session and
// nil error, or nil and a non-nil error — never both.
//
// On success the returned Session is ready for task execution.
func (m *Manager) Create(ctx context.Context, msg *protocol.AssignMsg) (*Session, error) {
	sessionID := uuid.New().String()

	workDir := filepath.Join(m.dataDir, "sessions", sessionID)
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return nil, fmt.Errorf("session %s: create working directory %q: %w", sessionID, workDir, err)
	}

	m.logger.InfoContext(
		ctx, "session: created",
		slog.String("session_id", sessionID),
		slog.String("work_dir", workDir),
		slog.String("job_id", msg.JobID),
	)

	// Write the OpenJD path-mapping file once, here at session creation, so that
	// BOTH environment actions (entered below) and the task action (run later by
	// the executor) can rely on Session.PathMappingRulesFile pointing at a real
	// file. The executor no longer writes it, avoiding a double-write.
	// An empty PathMap produces no file (WritePathMappingFile is a no-op).
	hasPathMap := len(msg.PathMap) > 0
	var pathMapFile string
	if hasPathMap {
		pathMapFile = filepath.Join(workDir, pathmap.PathMappingFileName)
		if err := pathmap.WritePathMappingFile(workDir, msg.PathMap); err != nil {
			if rmErr := os.RemoveAll(workDir); rmErr != nil {
				m.logger.WarnContext(
					ctx, "session: failed to remove working directory after path-map write failure",
					slog.String("session_id", sessionID),
					slog.String("work_dir", workDir),
					slog.Any("error", rmErr),
				)
			}
			return nil, fmt.Errorf("session %s: write path-mapping file: %w", sessionID, err)
		}
	}

	s := &Session{
		ID:             sessionID,
		WorkDir:        workDir,
		JobID:          msg.JobID,
		CreatedAt:      time.Now(),
		jobParams:      msg.JobParameters,
		pathMapFile:    pathMapFile,
		hasPathMap:     hasPathMap,
		dynamicEnv:     make(map[string]string),
		dynamicUnset:   make(map[string]bool),
		redactedValues: make(map[string]bool),
	}

	// Enter environments in declaration order.
	if err := s.enterEnvironments(ctx, msg.Environments, m.logger); err != nil {
		// enterEnvironments already ran reverse teardown on already-entered
		// environments. Remove the working directory so the caller gets a clean
		// nil, err return and does not need to call Cleanup on failure.
		if rmErr := os.RemoveAll(workDir); rmErr != nil {
			m.logger.WarnContext(
				ctx, "session: failed to remove working directory after setup failure",
				slog.String("session_id", sessionID),
				slog.String("work_dir", workDir),
				slog.Any("error", rmErr),
			)
		}
		return nil, fmt.Errorf("session %s: environment setup: %w", sessionID, err)
	}

	return s, nil
}

// Cleanup exits any remaining environments (if ExitEnvironments has not been
// called already) and removes the session's working directory.
//
// If failed is true and keepFailedSessions is set, the working directory is
// retained and a message is logged so the operator can inspect it.
//
// Cleanup logs all errors but does not return them so it can be called safely
// in deferred cleanup chains. Do not use the session after Cleanup returns.
func (m *Manager) Cleanup(ctx context.Context, s *Session, failed bool) {
	// Exit any remaining environments. If ExitEnvironments was already called
	// (e.g., by the cancellation path), s.enteredEnvs is nil and this is a no-op.
	if err := s.ExitEnvironments(ctx, m.logger); err != nil {
		m.logger.WarnContext(
			ctx, "session: environment teardown error during cleanup",
			slog.String("session_id", s.ID),
			slog.Any("error", err),
		)
	}

	// Retain the working directory on failure when the debug flag is set.
	if failed && m.keepFailedSessions {
		m.logger.InfoContext(
			ctx, "session: retaining failed session directory for inspection (--keep-failed-sessions is set)",
			slog.String("session_id", s.ID),
			slog.String("work_dir", s.WorkDir),
		)
		return
	}

	// Remove the working directory.
	if err := os.RemoveAll(s.WorkDir); err != nil {
		m.logger.WarnContext(
			ctx, "session: failed to remove working directory",
			slog.String("session_id", s.ID),
			slog.String("work_dir", s.WorkDir),
			slog.Any("error", err),
		)
	} else {
		m.logger.InfoContext(
			ctx, "session: working directory removed",
			slog.String("session_id", s.ID),
		)
	}
}

// ── Environment entry ─────────────────────────────────────────────────────────

// enterEnvironments enters each environment in envs in declaration order.
// If any OnEnter action fails, already-entered environments are torn down
// in reverse order before the error is returned.
func (s *Session) enterEnvironments(ctx context.Context, envs []protocol.AssignEnvironment, logger *slog.Logger) error {
	for _, env := range envs {
		if err := s.enterOne(ctx, env, logger); err != nil {
			// Abort: tear down already-entered environments in reverse.
			logger.WarnContext(
				ctx, "session: environment entry failed — tearing down entered environments",
				slog.String("session_id", s.ID),
				slog.String("failed_env", env.Name),
				slog.Any("error", err),
			)
			if teardownErr := s.ExitEnvironments(ctx, logger); teardownErr != nil {
				logger.WarnContext(
					ctx, "session: teardown after failed entry encountered additional errors",
					slog.String("session_id", s.ID),
					slog.Any("teardown_error", teardownErr),
				)
			}
			return fmt.Errorf("environment %q: %w", env.Name, err)
		}
	}
	return nil
}

// enterOne writes the environment's embedded files and runs its OnEnter action.
// On success, env is appended to s.enteredEnvs so ExitEnvironments includes it
// in teardown.
//
// enterOne is only called from enterEnvironments which runs during Create,
// before the session is returned to (and shared with) other goroutines.
// No lock is needed on s.enteredEnvs here; the mu lock is only required for
// activeTasks (modified during concurrent task execution) and the enteredEnvs
// clear in ExitEnvironments (called after Create returns).
func (s *Session) enterOne(ctx context.Context, env protocol.AssignEnvironment, logger *slog.Logger) error {
	// Build the environment-action scope (Param.*, RawParam.*,
	// Session.WorkingDirectory and Env.File.<name> for this environment's
	// embedded files — NOT Task.Param.*).  The File paths are computed first so
	// they are available before the onEnter action, the variable values, and the
	// embedded-file data are resolved against the same scope.
	scope, err := s.envScope(env.EmbeddedFiles)
	if err != nil {
		return fmt.Errorf("resolve environment: %w", err)
	}

	// Resolve {{...}} format strings in the onEnter action and the environment
	// variable values.  A bad reference fails the environment cleanly before any
	// side effects.
	resolvedEnter, err := fmtres.ResolveAction(env.OnEnter, scope)
	if err != nil {
		return fmt.Errorf("resolve environment: %w", err)
	}
	resolvedVars, err := fmtres.ResolveVars(env.Variables, scope)
	if err != nil {
		return fmt.Errorf("resolve environment: %w", err)
	}

	// Accumulate this environment's resolved static variables into the merged
	// session static environment (later environments override earlier ones, per
	// OpenJD declaration order). The executor reuses this for the task so static
	// env vars are resolved exactly once, against each env's own scope.
	if len(resolvedVars) > 0 {
		if s.staticEnv == nil {
			s.staticEnv = make(map[string]string, len(resolvedVars))
		}
		maps.Copy(s.staticEnv, resolvedVars)
	}

	// Resolve {{...}} references inside each embedded file's data against the
	// same environment scope before materializing it (so file data may reference
	// Param.*, Session.*, or another Env.File.* path).
	resolvedFiles, err := fmtres.ResolveEmbeddedFiles(env.EmbeddedFiles, scope)
	if err != nil {
		return fmt.Errorf("resolve embedded file data: %w", err)
	}

	// Write embedded files before OnEnter runs (environment files may be
	// consumed by the setup action, e.g., an activation script).
	if err := writeEmbeddedFiles(s.WorkDir, resolvedFiles); err != nil {
		return fmt.Errorf("write embedded files: %w", err)
	}

	if resolvedEnter != nil {
		logger.InfoContext(
			ctx, "session: entering environment",
			slog.String("session_id", s.ID),
			slog.String("env", env.Name),
		)
		envVars, unset := s.actionEnv(resolvedVars)
		if err := runAction(ctx, resolvedEnter, s.WorkDir, envVars, unset, logger, s.actionLineHandler(ctx, logger)); err != nil {
			return fmt.Errorf("on_enter: %w", err)
		}
		logger.InfoContext(
			ctx, "session: environment entered",
			slog.String("session_id", s.ID),
			slog.String("env", env.Name),
		)
	}

	// Record as entered so ExitEnvironments will include it in reverse-order teardown.
	// No lock needed — enterOne is only called from Create before the session is shared.
	s.enteredEnvs = append(s.enteredEnvs, env)
	return nil
}

// envScope builds the environment-action format-string scope: Param.<n> /
// RawParam.<n> (job parameters), Session.WorkingDirectory, the path-mapping
// variables, and Env.File.<name> for each of the given environment's embedded
// files. Task.Param.* is deliberately NOT in scope — environments are
// session-scoped, entered once, not per task.
//
// It returns an error naming the offending file when an embedded filename is
// invalid.
func (s *Session) envScope(files []protocol.EmbeddedFile) (fmtstring.MapScope, error) {
	scope := fmtres.EnvScope(s.jobParams, s.WorkDir, s.pathMapFile, s.hasPathMap)
	if err := fmtres.AddFileVars(scope, "Env.File", files, s.WorkDir); err != nil {
		return nil, err
	}
	return scope, nil
}

// resolveEnvAction resolves an environment's action (onEnter or onExit) and its
// variable values against the environment-action format-string scope built from
// the environment's embedded files (see [Session.envScope]).
//
// It returns the resolved action copy (nil when action is nil) and the resolved
// variable map (nil when vars is nil), or an error naming the offending
// reference when any value cannot be resolved.
func (s *Session) resolveEnvAction(action *protocol.Action, vars map[string]string, files []protocol.EmbeddedFile) (*protocol.Action, map[string]string, error) {
	scope, err := s.envScope(files)
	if err != nil {
		return nil, nil, err
	}
	resolvedAction, err := fmtres.ResolveAction(action, scope)
	if err != nil {
		return nil, nil, err
	}
	resolvedVars, err := fmtres.ResolveVars(vars, scope)
	if err != nil {
		return nil, nil, err
	}
	return resolvedAction, resolvedVars, nil
}

// ── Dynamic environment (openjd_env directives) ───────────────────────────────

const redactedPlaceholder = "<redacted>"

// applyEnvOp applies a parsed environment directive to the session's dynamic
// environment state. Safe for concurrent use.
func (s *Session) applyEnvOp(op openjd.EnvOp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch op.Kind {
	case openjd.EnvOpSet:
		if s.dynamicEnv == nil {
			s.dynamicEnv = make(map[string]string)
		}
		s.dynamicEnv[op.Name] = op.Value
		delete(s.dynamicUnset, op.Name)
		if op.Redacted && op.Value != "" {
			if s.redactedValues == nil {
				s.redactedValues = make(map[string]bool)
			}
			s.redactedValues[op.Value] = true
		}
	case openjd.EnvOpUnset:
		delete(s.dynamicEnv, op.Name)
		if s.dynamicUnset == nil {
			s.dynamicUnset = make(map[string]bool)
		}
		s.dynamicUnset[op.Name] = true
	}
}

// actionEnv merges the session's accumulated dynamic environment on top of the
// supplied static variables (dynamicEnv overrides static), and returns the
// merged override map plus the set of variables to unset. The caller passes both
// to envutil.BuildWithUnset so that unset variables are removed even when set by
// the static map or the inherited worker environment.
func (s *Session) actionEnv(staticVars map[string]string) (overrides map[string]string, unset map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dynamicEnv) == 0 && len(s.dynamicUnset) == 0 {
		return staticVars, nil
	}
	overrides = make(map[string]string, len(staticVars)+len(s.dynamicEnv))
	maps.Copy(overrides, staticVars)
	maps.Copy(overrides, s.dynamicEnv)
	if len(s.dynamicUnset) > 0 {
		unset = make(map[string]bool, len(s.dynamicUnset))
		maps.Copy(unset, s.dynamicUnset)
	}
	return overrides, unset
}

// StaticEnv returns a snapshot of the merged, fully-resolved static environment
// from all environments' Variables (see the staticEnv field). The executor uses
// it as the base environment for the task, on top of which the session's dynamic
// env (openjd_env directives) is applied. Returns nil when no environment sets
// any variable. The returned map is a clone; callers may mutate it freely.
func (s *Session) StaticEnv() map[string]string {
	if len(s.staticEnv) == 0 {
		return nil
	}
	return maps.Clone(s.staticEnv)
}

// EnvOverrides returns a snapshot of the environment variables accumulated from
// openjd_env / openjd_redacted_env directives, for callers (e.g. the executor's
// task-env construction) that must apply them on top of the task's static
// environment. Returns nil when no directives have set any variables.
func (s *Session) EnvOverrides() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dynamicEnv) == 0 {
		return nil
	}
	return maps.Clone(s.dynamicEnv)
}

// EnvUnset returns a snapshot of the variables removed via openjd_unset_env, for
// callers that must strip them from a subsequent action's environment. Returns
// nil when no variables have been unset.
func (s *Session) EnvUnset() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dynamicUnset) == 0 {
		return nil
	}
	return maps.Clone(s.dynamicUnset)
}

// actionLineHandler returns a per-action stdout/stderr line handler that
// processes OpenJD environment directives (stdout only) and returns the text
// that should be logged for the line. Directive side effects are applied to the
// session's dynamic environment; redacted directive lines and any line
// containing a known redacted value are scrubbed before logging.
func (s *Session) actionLineHandler(ctx context.Context, logger *slog.Logger) func(stream, line string) string {
	return func(stream, line string) string {
		if stream == "stdout" {
			if op, ok := openjd.ParseEnvDirective(line); ok {
				s.applyEnvOp(op)
				if op.Kind == openjd.EnvOpSet && op.Redacted {
					logger.DebugContext(ctx, "session: applied redacted env directive",
						slog.String("session_id", s.ID), slog.String("name", op.Name))
					return "openjd_redacted_env: " + op.Name + "=" + redactedPlaceholder
				}
				return line
			}
		}
		return s.scrubRedacted(line)
	}
}

// ScrubRedacted replaces any value registered via openjd_redacted_env in line
// with the redaction placeholder. It is the exported, concurrency-safe entry
// point the executor applies to every TASK stdout/stderr line: a step inherits
// redacted variables in its environment, so a command that echoes one (e.g.
// `echo $SECRET`) would otherwise publish the cleartext value to the log
// stream. Returns line unchanged when no redacted values are registered.
func (s *Session) ScrubRedacted(line string) string {
	return s.scrubRedacted(line)
}

// scrubRedacted replaces any known redacted value in line with the redaction
// placeholder so secrets never reach the logger. Safe for concurrent use.
//
// Matching is a plain substring replace, so a very short or common redacted
// value (e.g. "1") may over-redact unrelated output; callers should treat
// redacted values as opaque secrets, not arbitrary tokens.
func (s *Session) scrubRedacted(line string) string {
	s.mu.Lock()
	var vals []string
	for v := range s.redactedValues {
		vals = append(vals, v)
	}
	s.mu.Unlock()
	for _, v := range vals {
		if v != "" {
			line = strings.ReplaceAll(line, v, redactedPlaceholder)
		}
	}
	return line
}

// ── Action execution ──────────────────────────────────────────────────────────

// runAction starts action.Command as a child process and waits for it to exit.
//
// The process inherits the worker's environment with envVars merged on top
// (environment variables set for all actions in the session).
// workDir is the process working directory.
//
// If action.TimeoutSeconds > 0, a deadline is applied via a derived context.
// When ctx is canceled or the deadline elapses, exec.CommandContext sends
// SIGKILL to the child process. runAction then drains any remaining pipe output
// and calls Wait; it returns only after those steps complete. Callers should
// expect a brief delay between context cancellation and runAction returning,
// proportional to how quickly the OS kills the process and any remaining pipe
// data is read.
//
// stdout and stderr are captured concurrently (to avoid pipe-buffer deadlocks)
// and forwarded to the logger at debug level so operators can diagnose
// environment setup failures without flooding info logs.
//
// Each output line is offered to lineHandler, which processes recognized OpenJD
// environment directives (openjd_env / openjd_redacted_env / openjd_unset_env)
// on the "stdout" stream and returns the text to log for the line — redacting
// secret values so they never reach the logger. unset is passed through to
// envutil.BuildWithUnset so openjd_unset_env removes variables even when set by
// the static environment or the inherited worker environment. lineHandler may be
// nil, in which case lines are logged verbatim.
//
// A non-zero exit code is returned as an error that includes the exit code.
func runAction(ctx context.Context, action *protocol.Action, workDir string, envVars map[string]string, unset map[string]bool, logger *slog.Logger, lineHandler func(stream, line string) string) error {
	if action == nil || action.Command == "" {
		return nil
	}

	// Apply per-action timeout if specified.
	if action.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(action.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, action.Command, action.Args...) //nolint:gosec // command comes from the server-signed assignment
	cmd.Dir = workDir
	cmd.Env = envutil.BuildWithUnset(envVars, unset)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q: %w", action.Command, err)
	}

	// Read stdout and stderr concurrently to prevent pipe-buffer deadlocks:
	// if we read them sequentially and the process fills one buffer waiting for
	// the other to be drained, both sides block indefinitely. Lines are scanned
	// individually so OpenJD environment directives can be intercepted (and
	// secrets redacted) before the captured output is logged.
	var (
		stdoutText string
		stderrText string
		stdoutErr  error
		stderrErr  error
		readWg     sync.WaitGroup
	)
	readWg.Add(2)
	go func() {
		defer readWg.Done()
		stdoutText, stdoutErr = scanActionStream(stdout, "stdout", lineHandler)
	}()
	go func() {
		defer readWg.Done()
		stderrText, stderrErr = scanActionStream(stderr, "stderr", lineHandler)
	}()
	readWg.Wait()

	// Wait must be called after all pipe reads complete (Go docs: "it is
	// incorrect to call Wait before all reads from the pipe have completed").
	waitErr := cmd.Wait()

	if stdoutText != "" {
		logger.DebugContext(
			ctx,
			"session: env action stdout",
			slog.String("command", action.Command),
			slog.String("output", stdoutText),
		)
	}
	if stderrText != "" {
		logger.DebugContext(
			ctx,
			"session: env action stderr",
			slog.String("command", action.Command),
			slog.String("output", stderrText),
		)
	}

	// cmd.Wait() is the authoritative error signal (non-zero exit, signal
	// kill, timeout). Pipe read errors are reported only when Wait itself
	// succeeds, since a process kill closes the write end of the pipe and can
	// produce a transient read error that would otherwise obscure the real
	// cause.
	if waitErr != nil {
		return fmt.Errorf("command %q: %w", action.Command, waitErr)
	}
	if stdoutErr != nil {
		return fmt.Errorf("command %q: read stdout: %w", action.Command, stdoutErr)
	}
	if stderrErr != nil {
		return fmt.Errorf("command %q: read stderr: %w", action.Command, stderrErr)
	}
	return nil
}

// scanActionStream reads r line by line, passes each line through lineHandler
// (when non-nil) to obtain the loggable text, and returns the accumulated
// (possibly redacted) output plus any non-EOF scan error. Reading line by line
// lets the session intercept OpenJD environment directives and redact secrets
// before any output reaches the logger.
//
// The scanner's per-token buffer is raised from the default 64 KB to 4 MB so
// that environment-action commands that emit long log lines (e.g. compiler
// output, base64-encoded payloads) do not trigger bufio.ErrTooLong and fail an
// otherwise-successful action. Directives are always short, so directive
// handling is unaffected by this limit.
//
// If a single line still exceeds 4 MB (bufio.ErrTooLong), the remaining bytes
// are drained via io.Discard so the child process does not receive SIGPIPE
// (which would cause a non-zero exit code even when the task logic succeeded),
// a truncated-line placeholder is passed to lineHandler, and nil is returned
// so the scanner error is not treated as an action failure.
func scanActionStream(r io.Reader, stream string, lineHandler func(stream, line string) string) (string, error) {
	var buf strings.Builder
	scanner := bufio.NewScanner(r)
	// Raise the per-token limit from the default 64 KB to 4 MB.
	const maxLineBytes = 4 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if lineHandler != nil {
			line = lineHandler(stream, line)
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// Drain the pipe so the child process does not receive SIGPIPE.
			_, _ = io.Copy(io.Discard, r) //nolint:errcheck // drain pipe after oversized line; error is not actionable
			// Emit a placeholder so operators can see that output was truncated.
			truncated := "[output line exceeded 4 MB — truncated]"
			if lineHandler != nil {
				truncated = lineHandler(stream, truncated)
			}
			buf.WriteString(truncated)
			buf.WriteByte('\n')
			return buf.String(), nil
		}
		return buf.String(), err
	}
	return buf.String(), nil
}

// ── Embedded file writer ──────────────────────────────────────────────────────

// writeEmbeddedFiles materializes each file in files into workDir.
func writeEmbeddedFiles(workDir string, files []protocol.EmbeddedFile) error {
	for _, f := range files {
		if err := writeEmbeddedFile(workDir, f); err != nil {
			return fmt.Errorf("embedded file %q: %w", f.Name, err)
		}
	}
	return nil
}

// writeEmbeddedFile writes a single [protocol.EmbeddedFile] to workDir.
// The on-disk filename is computed by [fmtres.EmbeddedFileName] — the same
// helper that builds the Task.File.* / Env.File.* path variables — so the file
// is written exactly where those variables point. Filenames that contain path
// separators or null bytes are rejected to prevent directory traversal.
func writeEmbeddedFile(workDir string, f protocol.EmbeddedFile) error {
	name, err := fmtres.EmbeddedFileName(f)
	if err != nil {
		return err
	}

	data := []byte(applyEOL(f.Data, f.EndOfLine))

	perm := os.FileMode(0o640)
	if f.Runnable {
		perm = 0o750
	}

	path := filepath.Join(workDir, name)
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}

// applyEOL converts line endings in data according to eol.
//
//   - "LF"   → normalise all line endings to \n
//   - "CRLF" → normalise all line endings to \r\n
//   - "AUTO" or "" → return data unchanged
func applyEOL(data, eol string) string {
	switch eol {
	case "LF":
		out := make([]byte, 0, len(data))
		i := 0
		for i < len(data) {
			if data[i] == '\r' {
				out = append(out, '\n')
				if i+1 < len(data) && data[i+1] == '\n' {
					i += 2 // skip the \r\n pair together
				} else {
					i++ // lone \r → \n
				}
			} else {
				out = append(out, data[i])
				i++
			}
		}
		return string(out)

	case "CRLF":
		out := make([]byte, 0, len(data)+len(data)/10)
		i := 0
		for i < len(data) {
			switch data[i] {
			case '\r':
				out = append(out, '\r', '\n')
				if i+1 < len(data) && data[i+1] == '\n' {
					i += 2 // \r\n is already a CRLF pair
				} else {
					i++ // lone \r → CRLF
				}
			case '\n':
				out = append(out, '\r', '\n')
				i++
			default:
				out = append(out, data[i])
				i++
			}
		}
		return string(out)

	default:
		return data
	}
}
