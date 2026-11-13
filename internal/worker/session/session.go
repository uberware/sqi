// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session manages the lifecycle of OpenJD sessions on the worker.
//
// A Session is the ephemeral execution context within which one or more
// tasks from the same job step run. It provides:
//   - An isolated working directory under <session_root>/<session_id>/
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
// # Scope
//
// Sessions are worker-side runtime state, not database rows. The server
// records the session ID only on task_attempts. There is no sessions table
// (for session-reuse scheduling), and a session runs exactly one task; see
// docs/architecture.md ("SQLite schema overview").
//
// # Usage
//
//	mgr := session.NewManager(sessionRoot, cfg.Worker.KeepFailedSessions, provider, cfg.Isolation, limits, logger)
//	// sessionRoot is resolved by cmd/sqi-worker's effectiveSessionRoot; see
//	// its doc for why it is not cfg.Worker.DataDir.
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
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd/fmtstring"
	"github.com/uberware/sqi/internal/worker/commands"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/procout"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// applyCredential attaches an isolation.Credential to a process before it
// starts. Defaults to isolation.Apply; tests override it to record which
// call sites attach a credential without a real OS identity switch. Real
// uid/gid switching is exercised by make test-isolation.
var applyCredential = isolation.Apply

// ── Session ───────────────────────────────────────────────────────────────────

// Session is the ephemeral execution context for one or more tasks from the
// same job step. Obtain one via [Manager.Create]; end it by calling
// [Session.ExitEnvironments] followed by [Manager.Cleanup].
//
// Session is safe for concurrent use. Multiple tasks may execute within a
// single session simultaneously.
type Session struct {
	// ID is the worker-generated unique identifier for this session.
	// Callers include this in every [protocol.TaskStatusMsg] (as
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

	// msg is the assignment that created this session, and the only task
	// dispatched into it: sessions are not reused across tasks (see the
	// package comment). resolveEnvParts reads msg.EXPR to choose between the
	// base-spec fmtres.EnvScope/ResolveAction path and the phase-3
	// fmtres.EnvSymbols/ResolveActionExpr path, and reads msg.PathMap for the
	// EXPR path's metering and path-mapping options. Never mutated after
	// Create, so it is read without the mu lock, like
	// jobParams/pathMapFile/hasPathMap above.
	//
	// Non-nil for every Session: Manager.Create dereferences msg
	// (msg.Isolation) before it builds one, so resolveEnvParts reads s.msg
	// without a nil check.
	msg *protocol.AssignMsg

	// exprBudget is the one [fmtres.AssignmentBudget] shared by every live,
	// entry-time phase-3 table of this assignment. Manager.Create allocates
	// it before enterEnvironments runs, so every environment table built
	// during session creation shares it, and the executor's task-table
	// resolution (internal/worker/executor's resolveAssignmentExpr, via
	// [Session.ExprBudget]) reuses it. One budget per session, not per table
	// and not per worker process, bounds an assignment's total entry-time
	// phase-3 retained bytes and resolved positions across every table it
	// builds; see fmtres.AssignmentBudget's doc comment for the choice of
	// scope and how worker concurrency affects its size.
	//
	// The teardown path (resolveEnvAction, reached only via
	// ExitEnvironments) does not charge this budget; see that method's doc
	// comment.
	//
	// Non-nil for every Session: Create sets it in the struct literal it
	// returns.
	exprBudget *fmtres.AssignmentBudget

	// staticEnv is the merged, fully-resolved static environment from every
	// environment's Variables, accumulated in enterOne. Each environment's
	// values are resolved against that environment's own Env.File.* scope,
	// and later environments override earlier ones.
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
	// These apply to all subsequent actions in the session (later environments'
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

	// cred is the run-as-user identity every action runs under, or nil.
	//
	// Sessions are not reused (see the package comment). Any session reuse
	// must not cross run-as-user identities, so a reuse key must include
	// cred's identity: a session accumulates environment state (static
	// Environment.variables and dynamic openjd_env exports) for its lifetime,
	// and reusing it across identities hands one user's exported secrets to
	// another user's tasks.
	cred *isolation.Credential

	// baseEnv is the daemon environment already filtered by the worker's
	// isolation.env_passthrough allowlist, computed once at session
	// creation. Job-supplied variables (static Environment.variables, dynamic
	// openjd_env exports, task template vars) are layered on top at action
	// time via envutil.BuildFromBase and are never filtered; only what is
	// inherited from the daemon is subject to the allowlist. Equal to the full
	// (unfiltered) daemon environment when the assignment carries no
	// run_as_user isolation, so the no-isolation path inherits the daemon
	// environment byte for byte.
	baseEnv map[string]string

	// commands is the Manager's worker.commands resolver, or nil.
	commands *commands.Resolver
}

// ResolveAction returns the action to start for a: the same one when its
// command has no worker.commands entry, otherwise a copy that runs the entry's
// executable with the entry's arguments ahead of the action's own. a itself is
// never changed: it belongs to the assignment message, which a retry reuses.
//
// Both places that start job-supplied commands call this: the executor for a
// task's onRun and runAction's callers for an environment's onEnter/onExit.
// Any new launch site must call it too, or it ignores the operator's mapping.
func (s *Session) ResolveAction(a *protocol.Action) *protocol.Action {
	if a == nil {
		return nil
	}
	command, args := s.commands.Resolve(a.Command, a.Args)
	if command == a.Command && len(args) == len(a.Args) {
		return a
	}
	out := *a
	out.Command, out.Args = command, args
	return &out
}

// PathMappingRulesFile returns the absolute path to the OpenJD path-mapping
// file written into the session working directory, or "" when the assignment
// carried no path-mapping rules. Exposed to format strings as
// Session.PathMappingRulesFile.
func (s *Session) PathMappingRulesFile() string { return s.pathMapFile }

// HasPathMappingRules reports whether the assignment carried any path-mapping
// rules. Exposed to format strings as Session.HasPathMappingRules.
func (s *Session) HasPathMappingRules() bool { return s.hasPathMap }

// ExprBudget returns this assignment's shared [fmtres.AssignmentBudget] (see
// the exprBudget field). Callers resolving this session's task action
// (internal/worker/executor) or an environment's entry-side actions (enterOne,
// via resolveEnvEntry) pass this object to every fmtres phase-3 call, so the
// assignment's total live resolved-position and retained-byte spend is
// charged in one place. resolveEnvAction, the teardown path reached via
// ExitEnvironments, does not use it; see that method's doc comment.
func (s *Session) ExprBudget() *fmtres.AssignmentBudget { return s.exprBudget }

// WriteEmbeddedFiles materializes files into the session working directory.
// It is called by the executor before running the step OnRun action so that
// step-level embedded files are available to the task command.
//
// Each file's Runnable flag is honored (execute permission set) and its
// EndOfLine value is applied before writing. Returns an error naming the
// first file that could not be written, which the caller uses to fail the
// task through its pre-execution failure path.
func (s *Session) WriteEmbeddedFiles(files []protocol.EmbeddedFile) error {
	return writeEmbeddedFiles(s.WorkDir, files, s.cred)
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
// entry order, whatever the session's outcome.
//
// All teardown actions are attempted even if an earlier one fails. Errors
// from individual OnExit actions are logged as warnings and the first one is
// returned.
//
// ExitEnvironments is idempotent: the first call clears the
// entered-environments list, so later calls are no-ops. [Manager.Cleanup]
// always calls ExitEnvironments, so a caller that calls it beforehand (e.g.
// on the cancellation path) still calls Cleanup.
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
		// variable values against the environment-action scope. Teardown of the
		// remaining environments continues if one fails to resolve.
		resolvedExit, resolvedVars, err := s.resolveEnvAction(env, env.OnExit, env.Variables)
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
		if err := runAction(ctx, s.ResolveAction(resolvedExit), s.WorkDir, s.buildActionEnv(resolvedVars, nil), s.cred, logger, s.actionLineHandler(ctx, logger)); err != nil {
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
	// sessionRoot is the directory under which <sessionID> working
	// directories are created directly (no further nesting). It is not the
	// worker's data_dir: see workerconfig.LoadOrCreateWorkerID's doc and
	// cmd/sqi-worker's effectiveSessionRoot for why the two must differ.
	sessionRoot string
	// sessionRootMode is the mode sessionRoot is created at when it does not
	// already exist (os.MkdirAll never changes an existing directory's mode;
	// see prepareSessionsDir). Defaults to 0711, traversable from creation,
	// unless overridden via WithSessionRootMode. cmd/sqi-worker's
	// effectiveSessionRoot passes 0750 for the non-root fallback under
	// data_dir: run-as-user isolation needs root whatever the directory
	// permissions, so that location gains nothing from 0711.
	sessionRootMode    os.FileMode
	keepFailedSessions bool
	// provider resolves run-as-user credentials for isolated assignments.
	// Never invoked when an assignment's Isolation field is nil.
	provider isolation.Provider
	// isolationCfg.EnvPassthrough governs which daemon environment variables
	// an isolated session may additionally inherit beyond the minimal base
	// (see envutil.Policy). The other IsolationConfig fields (Required,
	// Provider) govern worker boot behavior and provider selection, handled
	// by the caller before NewManager is invoked.
	isolationCfg workerconfig.IsolationConfig
	// exprLimits is the operator's phase-3 expression budget for this host.
	// Every session this Manager creates gets its own
	// [fmtres.AssignmentBudget] built from it, and every phase-3 evaluation
	// the session performs reads its limits back off that budget. The zero
	// value normalizes to fmtres.DefaultExprLimits(), so a caller with no
	// configuration to offer meters by the defaults.
	exprLimits fmtres.ExprLimits
	logger     *slog.Logger
	// commands maps a command name a job runs to the executable to run in its
	// place (worker.commands). Nil resolves nothing. Handed to every session so
	// both launch sites (the executor's task action and an environment's
	// onEnter/onExit) resolve a name the same way.
	commands *commands.Resolver
}

// ManagerOption customizes a Manager constructed by NewManager, e.g.
// WithSessionRootMode or WithCommands.
type ManagerOption func(*Manager)

// WithSessionRootMode overrides the mode sessionRoot is created at (see the
// Manager.sessionRootMode field doc). It has no effect on a session root that
// already exists, because os.MkdirAll never changes an existing directory's
// mode.
func WithSessionRootMode(mode os.FileMode) ManagerOption {
	return func(m *Manager) { m.sessionRootMode = mode }
}

// WithCommands gives every session this Manager creates the operator's
// worker.commands resolver; see Session.ResolveAction.
func WithCommands(r *commands.Resolver) ManagerOption {
	return func(m *Manager) { m.commands = r }
}

// NewManager returns a Manager that stores session working directories
// directly under sessionRoot (<sessionRoot>/<sessionID>/). The caller
// (cmd/sqi-worker's effectiveSessionRoot) resolves sessionRoot to a location
// separate from the worker's data_dir.
// keepFailedSessions controls whether working directories for failed
// sessions are retained for post-mortem inspection. provider resolves
// run-as-user credentials for isolated assignments (see Manager.Create);
// isolationCfg.EnvPassthrough is the additional daemon environment allowlist
// for isolated sessions. exprLimits is this host's operator-configured
// phase-3 expression budget (cmd/sqi-worker maps the worker config's expr:
// section to it; the zero value means "the built-in defaults"). opts may
// include WithSessionRootMode to override the default 0711 creation mode.
func NewManager(sessionRoot string, keepFailedSessions bool, provider isolation.Provider, isolationCfg workerconfig.IsolationConfig, exprLimits fmtres.ExprLimits, logger *slog.Logger, opts ...ManagerOption) *Manager {
	m := &Manager{
		sessionRoot:        sessionRoot,
		sessionRootMode:    0o711,
		keepFailedSessions: keepFailedSessions,
		provider:           provider,
		isolationCfg:       isolationCfg,
		exprLimits:         exprLimits,
		logger:             logger,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// resolveCredential resolves the run-as-user credential for an isolated
// assignment and secures workDir for it (chown + chmod 0700; see
// isolation.SecureWorkDir). Returns (nil, nil) when the assignment carries no
// isolation.
//
// On failure it removes workDir itself, so Create can return (nil, err)
// without further cleanup. It never falls back to the daemon identity: that
// would run job code privileged while looking isolated.
func (m *Manager) resolveCredential(ctx context.Context, msg *protocol.AssignMsg, sessionID, workDir string) (*isolation.Credential, error) {
	if msg.Isolation == nil {
		return nil, nil
	}
	cred, err := m.provider.Resolve(ctx, isolation.Spec{
		User:  msg.Isolation.User,
		Group: msg.Isolation.Group,
	})
	if err != nil {
		if rmErr := os.RemoveAll(workDir); rmErr != nil {
			m.logger.WarnContext(ctx, "session: cleanup after credential failure",
				slog.String("session_id", sessionID), slog.Any("error", rmErr))
		}
		return nil, fmt.Errorf("session %s: resolve run-as-user %q: %w",
			sessionID, msg.Isolation.User, err)
	}
	if err := isolation.SecureWorkDir(workDir, cred); err != nil {
		// Every path that obtains a credential either returns it for the
		// session to own and close, or closes it before returning an error.
		// The caller never sees this one, so close it here.
		closeCredential(ctx, cred, sessionID, m.logger)
		if rmErr := os.RemoveAll(workDir); rmErr != nil {
			m.logger.WarnContext(ctx, "session: cleanup after secure-workdir failure",
				slog.String("session_id", sessionID), slog.Any("error", rmErr))
		}
		return nil, fmt.Errorf("session %s: secure working directory: %w", sessionID, err)
	}
	return cred, nil
}

// closeCredentialFn calls cred.Close(). Tests swap it for a counting wrapper
// to assert Close runs exactly once per session on every path that owns a
// credential, including each error path in Manager.Create as well as
// Manager.Cleanup. It is a seam like applyCredential. POSIX's
// Credential.Close is a no-op, so a missing call is invisible there; on
// Windows Close unloads a mounted profile hive and closes a token handle,
// and a missing call leaks both for the worker's lifetime.
var closeCredentialFn = func(cred *isolation.Credential) error { return cred.Close() }

// closeCredential closes cred if non-nil, logging rather than returning any
// error, like the other best-effort cleanup helpers in this file. A nil cred
// (no run_as_user isolation) is a no-op.
//
// Every path that releases a credential goes through here. On POSIX it has
// no observable effect; on Windows it releases a token handle and unloads a
// mounted profile hive.
func closeCredential(ctx context.Context, cred *isolation.Credential, sessionID string, logger *slog.Logger) {
	if cred == nil {
		return
	}
	if err := closeCredentialFn(cred); err != nil {
		logger.WarnContext(ctx, "session: failed to close run-as-user credential",
			slog.String("session_id", sessionID), slog.Any("error", err))
	}
}

// prepareSessionsDir ensures m.sessionRoot (and any missing parent) exists,
// validates its ancestors when the assignment is isolated, and returns it
// unchanged for Create to build <sessionID> under. It is a
// separate function to keep Create within the cyclomatic-complexity limit.
//
// The directory is created at m.sessionRootMode (0711 by default: search for
// everyone, nothing readable or writable beyond the owner; see
// WithSessionRootMode) rather than created narrow and widened afterward.
// os.MkdirAll never changes the mode of a directory that already exists, so
// this affects only a directory this call creates; an existing directory's
// mode is never widened to make it traversable. m.sessionRoot is never
// the worker's data_dir or an ancestor shared with the persistent worker-id
// file; see cmd/sqi-worker's effectiveSessionRoot and
// workerconfig.LoadOrCreateWorkerID.
//
// Ownership stays with the daemon (root): many run-as-user identities share
// this directory, so chowning it to one would grant that user nothing
// (search does not require ownership) and break every other user's sessions.
//
// isolated is true exactly when msg.Isolation != nil. cmd/sqi-worker's
// boot-time validateIsolationAncestors runs only when isolation.required is
// set, so every other isolated assignment is validated here, with the same
// check and message: a non-traversable ancestor (e.g. an operator-chosen
// worker.session_dir narrower than expected) fails only this task, not the
// whole worker. A non-isolated assignment skips the check: no other identity
// needs to chdir through sessionsDir.
func (m *Manager) prepareSessionsDir(sessionID string, isolated bool) (string, error) {
	if err := os.MkdirAll(m.sessionRoot, m.sessionRootMode); err != nil {
		return "", fmt.Errorf("session %s: create session root %q: %w", sessionID, m.sessionRoot, err)
	}
	if isolated {
		if err := isolation.ValidateTraversable(m.sessionRoot); err != nil {
			return "", fmt.Errorf("session %s: %w", sessionID, err)
		}
	}
	return m.sessionRoot, nil
}

// Create allocates a new Session for the given assignment: generates a UUID
// as the session ID, creates the working directory under
// <session_root>/<session_id>/, and enters each environment in
// declaration order.
//
// If any OnEnter action fails, already-entered environments are exited in
// reverse order and the working directory is removed before the
// error is returned. Create returns either a ready-to-use session and a nil
// error, or nil and a non-nil error.
//
// On success the returned Session is ready for task execution.
func (m *Manager) Create(ctx context.Context, msg *protocol.AssignMsg) (*Session, error) {
	sessionID := uuid.New().String()

	sessionsDir, err := m.prepareSessionsDir(sessionID, msg.Isolation != nil)
	if err != nil {
		return nil, err
	}

	// WorkDir must be absolute: it becomes cmd.Dir for every action and the base
	// for Task.File.* / Env.File.* paths. A relative session root (e.g. under the
	// Makefile's ./.run/workers) would otherwise yield a relative command path
	// that the child resolves against cmd.Dir, doubling the path and failing
	// with "no such file or directory".
	workDir, err := filepath.Abs(filepath.Join(sessionsDir, sessionID))
	if err != nil {
		return nil, fmt.Errorf("session %s: resolve absolute working directory: %w", sessionID, err)
	}
	// A workDir that will run job code under a different OS identity is
	// created 0700 (owner-only) rather than narrowed afterward. Unlike
	// sessionsDir, this directory belongs to one session and one target user
	// and must stay private; SecureWorkDir (called via resolveCredential
	// below) then chowns it to cred's uid/gid.
	workDirMode := os.FileMode(0o750)
	if msg.Isolation != nil {
		workDirMode = 0o700
	}
	if err := os.MkdirAll(workDir, workDirMode); err != nil {
		return nil, fmt.Errorf("session %s: create working directory %q: %w", sessionID, workDir, err)
	}

	// Resolve the OS identity before any job-supplied code runs. onEnter is
	// job code, one of the two launch sites that must carry a credential (the
	// executor's task-process launch is the other), and must never run as the
	// daemon while looking isolated.
	cred, err := m.resolveCredential(ctx, msg, sessionID, workDir)
	if err != nil {
		return nil, err
	}

	// Build the session's filtered base environment once, here. See the
	// baseEnv field doc: job-supplied variables are layered on top at action
	// time and are never subject to this filter. Policy.Enabled is false when
	// cred is nil, which makes BaseEnv return the full, unfiltered daemon
	// environment.
	envPolicy := envutil.Policy{
		Enabled:   cred != nil,
		Allowlist: m.isolationCfg.EnvPassthrough,
	}
	baseEnv := envutil.BaseEnv(envPolicy)
	if cred != nil {
		// Point HOME/USERPROFILE at the target user; the daemon's own is
		// unwritable to them, and DCCs write configs wherever these point.
		// The resolved identity can have no home directory (NSS resolution can
		// come back empty; see isolation.unixProvider.resolveIdentityViaNSS),
		// and keeping envutil.BaseEnv's inherited HOME would then hand the
		// task the daemon's home (typically /root), which this identity cannot
		// write. Fall back to the session's working directory, which
		// SecureWorkDir (called by resolveCredential above) made cred's own.
		home := cred.Home
		if home == "" {
			home = workDir
		}
		baseEnv["HOME"] = home
		baseEnv["USERPROFILE"] = home

		// APPDATA/LOCALAPPDATA are derived from the target's own profile, never
		// inherited: minimalBase excludes them, because the daemon's values
		// point into SYSTEM's profile and DCCs write preferences, license
		// caches, and crash dumps through them.
		if runtime.GOOS == "windows" {
			baseEnv["APPDATA"] = filepath.Join(home, "AppData", "Roaming")
			baseEnv["LOCALAPPDATA"] = filepath.Join(home, "AppData", "Local")
		}
	}

	m.logger.InfoContext(
		ctx, "session: created",
		slog.String("session_id", sessionID),
		slog.String("task_id", msg.TaskID),
		slog.String("attempt_id", msg.AttemptID),
		slog.String("work_dir", workDir),
		slog.String("job_id", msg.JobID),
	)

	// Write the OpenJD path-mapping file once, at session creation, so that
	// both environment actions (entered below) and the task action (run later
	// by the executor) find a real file at Session.PathMappingRulesFile. The
	// executor does not write it. It is written only when the
	// translation_file delivery is enabled; an assignment with no deliveries
	// set gets it whenever it carries rules.
	hasPathMap := len(msg.PathMap) > 0 && translationFileEnabled(msg.PathDeliveries)
	var pathMapFile string
	if hasPathMap {
		pathMapFile = filepath.Join(workDir, pathmap.PathMappingFileName)
		if err := pathmap.WritePathMappingFile(workDir, msg.PathMap); err != nil {
			closeCredential(ctx, cred, sessionID, m.logger)
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
		msg:            msg,
		dynamicEnv:     make(map[string]string),
		dynamicUnset:   make(map[string]bool),
		redactedValues: make(map[string]bool),
		cred:           cred,
		baseEnv:        baseEnv,
		commands:       m.commands,
		// Fresh per assignment, and set before enterEnvironments runs so
		// every environment table built during entry shares it. See the
		// exprBudget field.
		exprBudget: fmtres.NewAssignmentBudget(m.exprLimits),
	}

	// Enter environments in declaration order.
	if err := s.enterEnvironments(ctx, msg.Environments, m.logger); err != nil {
		// enterEnvironments already tore down the entered environments in
		// reverse. Remove the working directory so the caller gets nil, err
		// and need not call Cleanup. The session is discarded, so close its
		// credential here.
		closeCredential(ctx, cred, sessionID, m.logger)
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

	// The session's run-as-user credential (if any) is not needed after the
	// session ends; this is where the normal path closes it, as Create's
	// error paths do. It is closed even when keepFailedSessions retains the
	// working directory below: files kept for inspection do not need the
	// credential handle.
	closeCredential(ctx, s.cred, s.ID, m.logger)

	// Retain the working directory on failure when the debug flag is set.
	if failed && m.keepFailedSessions {
		m.logger.InfoContext(
			ctx, "session: retaining failed session directory for inspection (worker.keep_failed_sessions is set)",
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
// enterOne is called only from enterEnvironments, during Create, before the
// session is shared with other goroutines, so it appends to s.enteredEnvs
// without the mu lock. The lock guards activeTasks (changed during
// concurrent task execution) and the enteredEnvs clear in ExitEnvironments.
func (s *Session) enterOne(ctx context.Context, env protocol.AssignEnvironment, logger *slog.Logger) error {
	resolvedEnter, resolvedVars, resolvedFiles, err := s.resolveEnvEntry(env)
	if err != nil {
		return err
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

	// Write embedded files before OnEnter runs (environment files may be
	// consumed by the setup action, e.g., an activation script).
	if err := writeEmbeddedFiles(s.WorkDir, resolvedFiles, s.cred); err != nil {
		return fmt.Errorf("write embedded files: %w", err)
	}

	if resolvedEnter != nil {
		logger.InfoContext(
			ctx, "session: entering environment",
			slog.String("session_id", s.ID),
			slog.String("env", env.Name),
		)
		if err := runAction(ctx, s.ResolveAction(resolvedEnter), s.WorkDir, s.buildActionEnv(resolvedVars, nil), s.cred, logger, s.actionLineHandler(ctx, logger)); err != nil {
			return fmt.Errorf("on_enter: %w", err)
		}
		logger.InfoContext(
			ctx, "session: environment entered",
			slog.String("session_id", s.ID),
			slog.String("env", env.Name),
		)
	}

	// Record as entered so ExitEnvironments includes it in reverse-order
	// teardown. No lock: the session is not shared yet.
	s.enteredEnvs = append(s.enteredEnvs, env)
	return nil
}

// envScope builds the environment-action format-string scope: Param.<n> /
// RawParam.<n> (job parameters), Session.WorkingDirectory, the path-mapping
// variables, and Env.File.<name> for each of the given environment's embedded
// files. Task.Param.* is not in scope: environments are session-scoped and
// entered once, not per task.
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

// envFileResolver resolves an environment's embedded-file data against the
// scope (base spec) or symbol table (EXPR) that [Session.resolveEnvParts]
// built for that environment's action and variable values.
//
// It is a closure because only one of resolveEnvParts' two callers wants it:
// entry materializes files, teardown does not (see
// [Session.resolveEnvAction]). Each caller keeps its own error wrapping
// ("resolve embedded file data: %w" versus "resolve environment: %w").
type envFileResolver func([]protocol.EmbeddedFile) ([]protocol.EmbeddedFile, error)

// resolveEnvParts is the single implementation of environment resolution,
// shared by [Session.resolveEnvEntry] (onEnter, at Create) and
// [Session.resolveEnvAction] (onExit, at teardown). It resolves the action and
// the variable values against one environment-action scope or table and
// returns a closure that resolves embedded-file data against the same one.
//
// Entry and teardown must resolve the same way. If they differed, for example
// by folding in the step template's let: bindings ([fmtres.EnvSymbols]) only
// at entry, an onExit referencing a step-template binding would fail as an
// unknown symbol, and [Session.ExitEnvironments] logs such a failure as a
// warning and continues. Any OpenJD Template Schemas §3.6.2 ordering
// constraint belongs here.
//
// s.msg.EXPR selects the resolution family. A base-spec assignment (EXPR
// false, the zero value) takes the
// fmtres.EnvScope/ResolveAction/ResolveVars/ResolveEmbeddedFiles path, like
// [resolveAssignmentBaseSpec] in internal/worker/executor. When EXPR is true
// it builds the phase-3 symbol table (fmtres.EnvSymbols), evaluates env's own
// let: block into it exactly once (fmtres.ApplyEnvLet: a second call over the
// same table makes every binding fail the shadow check), and resolves the
// action, the Variables, and (via the returned closure) the EmbeddedFiles
// against that table via ResolveActionExpr/ResolveVarsExpr/
// ResolveEmbeddedFilesExpr.
//
// budget is a function, called once per fmtres call. Entry passes one that
// returns the shared s.exprBudget; teardown passes [Session.teardownBudget],
// which returns a fresh budget per call, so each teardown evaluation has its
// own ledger (see resolveEnvAction and teardownBudget). It is never called on
// the base-spec path, which meters nothing.
//
// Errors are returned unwrapped, except the let: block's, which are wrapped
// once as "let bindings: %w"; each caller adds its own context.
func (s *Session) resolveEnvParts(
	env protocol.AssignEnvironment,
	action *protocol.Action,
	vars map[string]string,
	budget func() *fmtres.AssignmentBudget,
) (*protocol.Action, map[string]string, envFileResolver, error) {
	if !s.msg.EXPR {
		// Build the environment-action scope (Param.*, RawParam.*,
		// Session.WorkingDirectory and Env.File.<name> for this environment's
		// embedded files, not Task.Param.*). The File paths are computed first
		// so the action, the variable values, and the embedded-file data can
		// all reference them.
		scope, err := s.envScope(env.EmbeddedFiles)
		if err != nil {
			return nil, nil, nil, err
		}

		// Resolve {{...}} format strings in the action and the environment
		// variable values. A bad reference fails the environment before any
		// side effects.
		resolvedAction, err := fmtres.ResolveAction(action, scope)
		if err != nil {
			return nil, nil, nil, err
		}
		resolvedVars, err := fmtres.ResolveVars(vars, scope)
		if err != nil {
			return nil, nil, nil, err
		}
		// Embedded-file data resolves against the same environment scope (so
		// file data may reference Param.*, Session.*, or another Env.File.*
		// path), for the caller that asks for it.
		return resolvedAction, resolvedVars, func(files []protocol.EmbeddedFile) ([]protocol.EmbeddedFile, error) {
			return fmtres.ResolveEmbeddedFiles(files, scope)
		}, nil
	}

	// EnvSymbols also folds in the enclosing step template's let-bound names
	// for a step environment (Template Schemas §3.6.2 row 1); see its doc
	// comment. This function does not apply that block itself. Without the
	// fold, an onExit action referencing a step-template binding is an
	// unknown symbol.
	syms, err := fmtres.EnvSymbols(s.msg, &env, s.WorkDir, s.pathMapFile, s.hasPathMap, budget())
	if err != nil {
		return nil, nil, nil, err
	}
	// Variables are resolved before this environment's own let: block is
	// bound: §3.6.2 row 4 grants those names to the script's children
	// (actions, embeddedFiles), and Variables is a sibling of Script, not a
	// descendant. Phase 2 checks Variables against the pre-let table
	// (checkEnvironmentExpressions, exprcheck.go), and this matches it at
	// phase 3. See fmtres.ApplyEnvLet's doc comment.
	resolvedVars, err := fmtres.ResolveVarsExpr(vars, syms, s.msg.PathMap, budget())
	if err != nil {
		return nil, nil, nil, err
	}
	// Exactly one call over this table (see fmtres.ApplyEnvLet's doc comment).
	// Both resolutions below, the action and then EmbeddedFiles, reuse syms.
	if err := fmtres.ApplyEnvLet(&env, syms, s.msg.PathMap, budget()); err != nil {
		return nil, nil, nil, fmt.Errorf("let bindings: %w", err)
	}
	resolvedAction, err := fmtres.ResolveActionExpr(action, syms, s.msg.PathMap, budget())
	if err != nil {
		return nil, nil, nil, err
	}
	return resolvedAction, resolvedVars, func(files []protocol.EmbeddedFile) ([]protocol.EmbeddedFile, error) {
		return fmtres.ResolveEmbeddedFilesExpr(files, syms, s.msg.PathMap, budget())
	}, nil
}

// resolveEnvEntry resolves everything enterOne needs for one environment
// entry (the onEnter action, the variable values, and the embedded files'
// data) against one environment-action scope or table. The resolution itself
// is [Session.resolveEnvParts]; this method supplies the entry-side budget
// (the shared s.exprBudget, charged across every table this assignment
// builds) and the entry-side error wrapping.
//
// Returns the resolved action copy (nil when env.OnEnter is nil), the
// resolved variable map, and the resolved embedded files, or an error
// naming the offending reference/file.
func (s *Session) resolveEnvEntry(env protocol.AssignEnvironment) (*protocol.Action, map[string]string, []protocol.EmbeddedFile, error) {
	resolvedEnter, resolvedVars, resolveFiles, err := s.resolveEnvParts(
		env, env.OnEnter, env.Variables,
		func() *fmtres.AssignmentBudget { return s.exprBudget },
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve environment: %w", err)
	}
	resolvedFiles, err := resolveFiles(env.EmbeddedFiles)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve embedded file data: %w", err)
	}
	return resolvedEnter, resolvedVars, resolvedFiles, nil
}

// resolveEnvAction resolves an environment's onExit action and its variable
// values against the environment-action format-string scope or table built
// from env; see [Session.resolveEnvParts], which it shares with
// [Session.resolveEnvEntry]. Unlike resolveEnvEntry it never resolves
// embedded-file data: its only caller, [Session.ExitEnvironments], does not
// re-materialize files on exit, and needs the scope only so onExit and
// Variables may reference an Env.File.* path. It discards resolveEnvParts'
// file-resolving closure.
//
// It returns the resolved action copy (nil when action is nil) and the resolved
// variable map (nil when vars is nil), or an error naming the offending
// reference when any value cannot be resolved.
//
// The EXPR path does not charge s.exprBudget. Teardown builds a new table
// and evaluates the environment's let: block a second time, so charging the
// shared assignment budget would count bytes already charged at entry, for a
// table already discarded. ExitEnvironments treats a resolve error as a
// warning and continues, so an exhausted budget would skip each remaining
// environment's onExit (license check-ins, daemon shutdowns, unmounts) with
// only a log line, while the allocation it charges for happens anyway. The
// per-table (LetRetainedBytes) and per-Eval (OperationLimit/MemoryLimit)
// bounds still apply to the exit-time table; only the assignment-wide ledger
// is exempt.
//
// It passes the method value [Session.teardownBudget], which resolveEnvParts
// calls once per evaluation, so each evaluation gets its own fresh budget
// with the host's configured limits. A single budget shared by the whole
// teardown would accumulate the evaluations' position charges against one
// cap, so a large environment that entered within the limit could fail at
// exit and have its onExit skipped.
func (s *Session) resolveEnvAction(
	env protocol.AssignEnvironment, action *protocol.Action, vars map[string]string,
) (*protocol.Action, map[string]string, error) {
	resolvedAction, resolvedVars, _, err := s.resolveEnvParts(env, action, vars, s.teardownBudget)
	if err != nil {
		return nil, nil, err
	}
	return resolvedAction, resolvedVars, nil
}

// teardownBudget returns a fresh [fmtres.AssignmentBudget] carrying this
// host's configured expression limits and an empty ledger, for one
// environment-teardown evaluation. Call it once per evaluation:
//
//   - Same limits: passing nil instead would meter teardown against the
//     built-in defaults rather than the operator's expr: configuration.
//   - Fresh per call: the assignment-wide ledger is not charged at teardown
//     (see [Session.resolveEnvAction]), and one budget shared across a
//     teardown's evaluations would accumulate their position charges against
//     one cap, so an environment that entered within the limit could fail
//     at exit.
func (s *Session) teardownBudget() *fmtres.AssignmentBudget {
	return fmtres.NewAssignmentBudget(s.exprBudget.Limits())
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
// to buildActionEnv (envutil.BuildFromBase) so that unset variables are removed
// even when set by the static map or the session's filtered base environment.
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

// ── Isolation ─────────────────────────────────────────────────────────────────

// Credential returns the OS identity every action in this session runs
// under, or nil when the assignment carried no run_as_user isolation, in
// which case every action runs as the worker daemon.
func (s *Session) Credential() *isolation.Credential { return s.cred }

// BaseEnv returns the daemon environment already filtered for this session by
// the worker's isolation.env_passthrough policy (see the baseEnv field). It
// governs only what is inherited from the daemon; callers layer job-supplied
// variables on top via envutil.BuildFromBase, which never re-filters them.
// The returned map must not be mutated by callers.
func (s *Session) BaseEnv() map[string]string { return s.baseEnv }

// buildActionEnv builds the final process environment for one action: the
// session's filtered base (BaseEnv), staticOverrides applied on top, then the
// session's accumulated dynamic environment (openjd_env directives) layered
// over that, then every key in unset (merged with any dynamic unsets)
// removed. Session actions (onEnter/onExit) build their command environment
// only here, so BaseEnv's filtering, applied once at session creation, is
// never applied to the job-data layers on top.
func (s *Session) buildActionEnv(staticOverrides map[string]string, unset map[string]bool) []string {
	overrides, dynUnset := s.actionEnv(staticOverrides)
	if len(dynUnset) > 0 {
		merged := make(map[string]bool, len(unset)+len(dynUnset))
		maps.Copy(merged, unset)
		maps.Copy(merged, dynUnset)
		unset = merged
	}
	return envutil.BuildFromBase(s.baseEnv, overrides, unset)
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
// with the redaction placeholder. The executor applies it to every task
// stdout/stderr line: a step inherits redacted variables in its environment,
// so a command that echoes one (e.g. `echo $SECRET`) would otherwise publish
// the cleartext value to the log stream. Safe for concurrent use. Returns
// line unchanged when no redacted values are registered.
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
// The process runs under cred (via isolation.Apply; a nil cred applies
// nothing) with env as its complete process environment, already built by
// the caller (see Session.buildActionEnv). workDir is the process working
// directory.
//
// If action.TimeoutSeconds > 0, a deadline is applied via a derived context.
// When ctx is canceled or the deadline elapses, exec.CommandContext kills the
// child process. runAction then waits for the process and reads the rest of
// its output, for at most procout.DrainDelay after the exit when a descendant
// still holds it open, and returns only after that, so it returns shortly
// after context cancellation rather than at once.
//
// stdout and stderr are read concurrently (to avoid pipe-buffer deadlocks)
// and logged at debug level, so operators can diagnose environment setup
// failures without flooding info logs.
//
// Each output line is offered to lineHandler, which processes recognized OpenJD
// environment directives (openjd_env / openjd_redacted_env / openjd_unset_env)
// on the "stdout" stream and returns the text to log for the line, with
// secret values redacted. lineHandler may be nil, in which case lines are
// logged verbatim.
//
// A non-zero exit code is returned as an error that includes the exit code.
func runAction(ctx context.Context, action *protocol.Action, workDir string, env []string, cred *isolation.Credential, logger *slog.Logger, lineHandler func(stream, line string) string) error {
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
	// This is job-supplied code (an OpenJD environment onEnter/onExit action)
	// and must run under the session's credential, not the daemon's. onEnter
	// runs before any task, so a missed credential here would run job code
	// privileged while every task around it looked isolated.
	if err := applyCredential(cmd, cred); err != nil {
		return fmt.Errorf("apply run-as-user credential: %w", err)
	}
	cmd.Env = env

	// procout, not StdoutPipe, so the action's exit is seen while a descendant
	// still holds its output open, such as a service an onEnter action starts
	// in the background.
	out, err := procout.Start(cmd)
	if err != nil {
		return fmt.Errorf("start %q: %w", action.Command, err)
	}

	// Read stdout and stderr concurrently: read one after the other, a
	// process that fills one stream while the other is unread blocks both
	// sides. Lines are scanned individually so OpenJD environment directives
	// are intercepted, and secrets redacted, before the output is logged.
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
		stdoutText, stdoutErr = scanActionStream(out.Stdout, "stdout", lineHandler)
	}()
	go func() {
		defer readWg.Done()
		stderrText, stderrErr = scanActionStream(out.Stderr, "stderr", lineHandler)
	}()
	// No process group is tracked for an environment action, so the drain
	// counts from its exit: a service it starts in the background outlives it
	// and keeps running.
	stillOpen, waitErr := out.Wait(nil)
	readWg.Wait()
	if stillOpen {
		logger.WarnContext(ctx,
			"session: an environment action exited but a process it started still holds the output; output after the drain delay is not captured",
			slog.String("command", action.Command), slog.Duration("waited", procout.DrainDelay))
	}

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

	// The wait error is the authoritative result (non-zero exit, signal kill,
	// timeout). Read errors are reported only when the wait succeeds, since a
	// process kill closes the write end of the pipe and can produce a read
	// error that would obscure the real cause.
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
// otherwise-successful action. Directives are short, so this limit does not
// affect them.
//
// If a single line still exceeds 4 MB (bufio.ErrTooLong), the remaining bytes
// are read into io.Discard so the child is not left blocked writing output
// nobody reads, a truncated-line placeholder is passed to lineHandler, and
// nil is returned so the scanner error is not treated as an action failure.
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
			// Drain the stream so the child is not blocked writing to it.
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

// writeEmbeddedFiles materializes each file in files into workDir. cred is the
// session's run-as-user credential (nil when the assignment carries no
// isolation), passed through to writeEmbeddedFile so each file ends up owned
// by the identity that must read or execute it.
func writeEmbeddedFiles(workDir string, files []protocol.EmbeddedFile, cred *isolation.Credential) error {
	for _, f := range files {
		if err := writeEmbeddedFile(workDir, f, cred); err != nil {
			return fmt.Errorf("embedded file %q: %w", f.Name, err)
		}
	}
	return nil
}

// writeEmbeddedFile writes a single [protocol.EmbeddedFile] to workDir.
// The on-disk filename is computed by [fmtres.EmbeddedFileName], the same
// helper that builds the Task.File.* / Env.File.* path variables, so the file
// is written where those variables point. Filenames that contain path
// separators or null bytes are rejected to prevent directory traversal.
//
// When cred is non-nil the file is chowned to cred's identity as part of the
// write. The mode (0640, or 0750 when Runnable) has no "other" bits, so
// without the chown an isolated task, being neither the owner nor in the
// group, could neither read a data file nor execute a runnable one. The mode
// is never
// widened to world-readable: these files can carry job data, and the chown
// makes them usable by the one identity that needs them.
//
// Step embedded files are written after job code has already run as the
// target uid (the environment onEnter action runs first), at a predictable
// name, so a hardlink or symlink planted at the path could redirect a
// path-based write or chown. isolation.WriteFileFchown prevents that: it
// removes any existing entry without following it, recreates the file with
// O_EXCL|O_NOFOLLOW, and chowns via fchown on the open descriptor, which acts
// on the inode rather than the path. A second embedded file with the same
// name (two environments, or an environment and the step, each declaring one
// named "run") overwrites the first, matching fmtres.AddFileVars's last-wins
// scope semantics. See WriteFileFchown's doc for the threat model. With a nil
// cred it does not chown.
func writeEmbeddedFile(workDir string, f protocol.EmbeddedFile, cred *isolation.Credential) error {
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
	if err := isolation.WriteFileFchown(path, data, perm, cred); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}

// translationFileEnabled reports whether the translation_file delivery is in the
// set. A nil or empty set counts as enabled, so the OpenJD path-mapping file
// is written.
func translationFileEnabled(deliveries []protocol.PathDelivery) bool {
	if len(deliveries) == 0 {
		return true
	}
	for _, d := range deliveries {
		if d.Kind == "translation_file" {
			return true
		}
	}
	return false
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
