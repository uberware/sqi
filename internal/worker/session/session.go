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
// # Phase 1 scope
//
// In Phase 1, sessions are not stored as database rows — they are worker-side
// runtime constructs. The server records the session ID only on task_attempts.
// A dedicated sessions table (for session-reuse scheduling) is deferred to
// Phase 2; see docs/architecture.md ("SQLite schema overview").
//
// # Usage
//
//	mgr := session.NewManager(sessionRoot, cfg.Worker.KeepFailedSessions, provider, cfg.Isolation, logger)
//	// sessionRoot is resolved by cmd/sqi-worker's effectiveSessionRoot — see
//	// its doc for why it is deliberately NOT cfg.Worker.DataDir.
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
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/pathmap"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// applyCredential attaches an isolation.Credential to a process before it
// starts. Defaults to isolation.Apply; tests override it to record which
// call sites attach a credential without needing a real OS identity switch
// (see isolation.NewFake's own doc: a fake cannot verify real uid/gid
// switching — that needs a real OS, exercised by make test-isolation).
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

	// HARD INVARIANT — session reuse (deferred, see the package comment at the
	// top of this file):
	//
	//   A SESSION MUST NOT BE REUSED ACROSS DIFFERENT RUN-AS-USER IDENTITIES.
	//   ANY REUSE KEY MUST INCLUDE cred's IDENTITY.
	//
	// A session accumulates environment state — static Environment.variables and
	// dynamic openjd_env exports — that persists for its lifetime. Reusing one
	// across identities hands one user's exported secrets to another user's
	// tasks, silently defeating isolation while every other control still looks
	// correct.
	cred *isolation.Credential

	// baseEnv is the daemon environment already filtered by the worker's
	// isolation.env_passthrough allowlist, computed ONCE here at session
	// creation. Job-supplied variables (static Environment.variables, dynamic
	// openjd_env exports, task template vars) are layered on top at action
	// time via envutil.BuildFromBase and are NEVER filtered — only what is
	// inherited from the daemon is subject to the allowlist. Equal to the full
	// (unfiltered) daemon environment when the assignment carries no
	// run_as_user isolation, so the no-isolation path is byte-for-byte
	// unchanged from before this field existed.
	baseEnv map[string]string
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
		if err := runAction(ctx, resolvedExit, s.WorkDir, s.buildActionEnv(resolvedVars, nil), s.cred, logger, s.actionLineHandler(ctx, logger)); err != nil {
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
	// directories are created DIRECTLY (no further nesting). Deliberately NOT
	// the worker's data_dir: see workerconfig.LoadOrCreateWorkerID's doc and
	// cmd/sqi-worker's effectiveSessionRoot for why the two must never be the
	// same directory again.
	sessionRoot string
	// sessionRootMode is the mode sessionRoot is created at when it does not
	// already exist (os.MkdirAll never touches an existing directory's mode —
	// see prepareSessionsDir). Defaults to 0711 (traversable-from-birth,
	// matching every call site that predates this field) unless overridden via
	// WithSessionRootMode — see cmd/sqi-worker's effectiveSessionRoot for why
	// production chooses 0750 instead for the non-root DataDir fallback (the
	// pre-split mode, restored for byte-for-byte backward compatibility: real
	// isolation cannot function without root regardless of directory
	// permissions, so that location gains nothing from the wider 0711).
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
	logger       *slog.Logger
}

// ManagerOption customizes a Manager constructed by NewManager. See
// WithSessionRootMode for the option currently defined.
type ManagerOption func(*Manager)

// WithSessionRootMode overrides the mode sessionRoot is created at (see the
// Manager.sessionRootMode field doc). Only takes effect the first time
// sessionRoot is created — os.MkdirAll never touches an existing directory's
// mode — so it has no effect on a session root that already exists.
func WithSessionRootMode(mode os.FileMode) ManagerOption {
	return func(m *Manager) { m.sessionRootMode = mode }
}

// NewManager returns a Manager that stores session working directories
// directly under sessionRoot (<sessionRoot>/<sessionID>/) — the caller (in
// production, cmd/sqi-worker's effectiveSessionRoot) has already resolved
// this to the right location, separate from the worker's data_dir.
// keepFailedSessions controls whether working directories for failed
// sessions are retained for post-mortem inspection. provider resolves
// run-as-user credentials for isolated assignments (see Manager.Create);
// isolationCfg.EnvPassthrough is the additional daemon environment allowlist
// for isolated sessions. opts may include WithSessionRootMode to override the
// default 0711 creation mode.
func NewManager(sessionRoot string, keepFailedSessions bool, provider isolation.Provider, isolationCfg workerconfig.IsolationConfig, logger *slog.Logger, opts ...ManagerOption) *Manager {
	m := &Manager{
		sessionRoot:        sessionRoot,
		sessionRootMode:    0o711,
		keepFailedSessions: keepFailedSessions,
		provider:           provider,
		isolationCfg:       isolationCfg,
		logger:             logger,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// resolveCredential resolves the run-as-user credential for an isolated
// assignment and secures workDir for it (chown + chmod 0700 — see
// isolation.SecureWorkDir). Returns (nil, nil) when the assignment carries no
// isolation, which is the pre-isolation default.
//
// On failure it removes workDir itself, so Create can return a clean
// (nil, err) without duplicating cleanup, and never falls back to the daemon
// identity: that would run job code privileged while looking isolated.
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
		// cred was already obtained above; this function is returning an
		// error instead of the credential, so it owns closing it here — the
		// caller never sees it to close it itself (Task 8's Credential
		// lifecycle: every path that obtains one either returns it for the
		// session to own and close, or closes it itself before erroring out).
		closeCredential(ctx, cred, sessionID, m.logger)
		if rmErr := os.RemoveAll(workDir); rmErr != nil {
			m.logger.WarnContext(ctx, "session: cleanup after secure-workdir failure",
				slog.String("session_id", sessionID), slog.Any("error", rmErr))
		}
		return nil, fmt.Errorf("session %s: secure working directory: %w", sessionID, err)
	}
	return cred, nil
}

// closeCredentialFn is the seam for cred.Close(), swappable in tests so a
// counting wrapper can assert Close is invoked exactly once per session
// across every path that owns a credential — including every error path in
// Manager.Create, not just the normal Manager.Cleanup path — mirroring the
// applyCredential-style seam already used for isolation.Apply in this
// package and internal/worker/executor. Without this seam nothing could
// distinguish a dropped Close() call from a correct one: both implementations
// of Credential.Close are currently no-ops, so a missing call is invisible
// until the Windows LogonUser provider starts holding a real token handle.
var closeCredentialFn = func(cred *isolation.Credential) error { return cred.Close() }

// closeCredential closes cred if non-nil, logging (not returning) any error —
// mirroring the other best-effort cleanup helpers in this file. A nil cred
// (no run_as_user isolation) is a safe no-op, matching every other credential
// helper's convention in this package.
//
// POSIX and the current Windows placeholder both implement Close as a no-op,
// so this has no observable effect today; it exists so the credential has a
// single, clear lifecycle owner before the real Windows LogonUser provider
// lands and starts holding an actual token handle that DOES need releasing.
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
// then — only for an isolated assignment — validates its ancestors, before
// returning it unchanged for Create to build <sessionID> under. Extracted
// from Create to keep that function's cyclomatic complexity within the
// project limit.
//
// Created directly at m.sessionRootMode (0711 by default — search for
// everyone, nothing readable/writable beyond the owner; see
// WithSessionRootMode for how production picks 0750 instead for the
// non-root DataDir fallback) rather than created narrow and widened
// afterward: os.MkdirAll never touches the mode of a directory that already
// exists, so this only ever affects a directory this call itself creates for
// the first time. That distinction is the whole point of this package's
// isolation split — creating traversable FROM BIRTH and mutating an
// EXISTING directory's mode to become traversable are different operations,
// and only the latter (what an earlier version of this function did to
// m.dataDir and dataDir/"sessions" unconditionally, isolated session or not)
// is the anti-pattern being eliminated. m.sessionRoot is never the worker's
// data_dir or an ancestor shared with the persistent worker-id file — see
// cmd/sqi-worker's effectiveSessionRoot and workerconfig.LoadOrCreateWorkerID's
// own doc for the split this depends on.
//
// Ownership is deliberately left as the daemon's (root): many different
// run-as-user identities share this one root directory, so chowning it to
// any single one would grant that user nothing (search doesn't require
// ownership) while breaking every other user's sessions.
//
// isolated is true exactly when msg.Isolation != nil — the moment the
// worker first learns an identity is in play. cmd/sqi-worker's boot-time
// validateIsolationAncestors only runs when isolation.required is set
// (being root and actually receiving an isolated assignment are not the
// same predicate), so every OTHER isolated assignment is validated HERE
// instead — the IDENTICAL check with the IDENTICAL actionable message — so
// a non-traversable ancestor (e.g. an operator-chosen worker.session_dir
// that is narrower than expected) fails only THIS task, not the whole
// worker. A non-isolated assignment skips this: no other identity will ever
// need to chdir through sessionsDir, so there is nothing to validate.
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
// error is returned. Create always returns either a ready-to-use session and
// nil error, or nil and a non-nil error — never both.
//
// On success the returned Session is ready for task execution.
func (m *Manager) Create(ctx context.Context, msg *protocol.AssignMsg) (*Session, error) {
	sessionID := uuid.New().String()

	sessionsDir, err := m.prepareSessionsDir(sessionID, msg.Isolation != nil)
	if err != nil {
		return nil, err
	}

	// WorkDir must be absolute: it becomes cmd.Dir for every action and the base
	// for Task.File.* / Env.File.* paths. A relative data_dir (e.g. the Makefile's
	// ./.run/workers/worker-N) would otherwise yield a relative command path that,
	// combined with cmd.Dir, the forked child resolves against the session dir
	// itself — doubling the path and failing with "no such file or directory".
	workDir, err := filepath.Abs(filepath.Join(sessionsDir, sessionID))
	if err != nil {
		return nil, fmt.Errorf("session %s: resolve absolute working directory: %w", sessionID, err)
	}
	// A workDir that will run job code under a different OS identity is
	// created 0700 from the start (owner-only) rather than relying solely on
	// the chown+chmod below to narrow it after the fact. Unlike sessionsDir
	// above, this leaf directory belongs to exactly one session/one target
	// user and MUST stay private — SecureWorkDir (called via
	// resolveCredential below) narrows it further to cred's own uid/gid.
	workDirMode := os.FileMode(0o750)
	if msg.Isolation != nil {
		workDirMode = 0o700
	}
	if err := os.MkdirAll(workDir, workDirMode); err != nil {
		return nil, fmt.Errorf("session %s: create working directory %q: %w", sessionID, workDir, err)
	}

	// Resolve the OS identity before any job-supplied code runs. onEnter is
	// job code (site 1 of the two launch sites that must carry a credential;
	// the executor's task-process launch is site 2) — it must never run as
	// the daemon while looking isolated.
	cred, err := m.resolveCredential(ctx, msg, sessionID, workDir)
	if err != nil {
		return nil, err
	}

	// Build the session's filtered base environment exactly once, here. See
	// the baseEnv field doc: job-supplied variables are layered on top at
	// action time and are never subject to this filter. Policy.Enabled is
	// false when cred is nil, which makes BaseEnv return the full (unfiltered)
	// daemon environment — the no-isolation path is byte-for-byte unchanged.
	envPolicy := envutil.Policy{
		Enabled:   cred != nil,
		Allowlist: m.isolationCfg.EnvPassthrough,
	}
	baseEnv := envutil.BaseEnv(envPolicy)
	if cred != nil {
		// Point HOME/USERPROFILE at the target user; the daemon's own is
		// unwritable to them, and DCCs write configs wherever these point.
		// When the resolved identity has no discoverable home directory
		// (envutil's own doc on minimalBase flags this as a MUST-rewrite: NSS
		// resolution can legitimately come back empty — see
		// isolation.unixProvider.resolveIdentityViaNSS), falling through to
		// envutil.BaseEnv's inherited HOME would silently hand the task the
		// DAEMON's home (typically /root) — writable by the daemon, not by
		// this task's identity, i.e. exactly the failure this rewrite exists
		// to prevent. Fall back to the session's own working directory
		// instead: it is always workDir-owned by cred (via SecureWorkDir,
		// called just above by resolveCredential) and therefore always
		// writable by the identity that needs a HOME.
		home := cred.Home
		if home == "" {
			home = workDir
		}
		baseEnv["HOME"] = home
		baseEnv["USERPROFILE"] = home

		// APPDATA/LOCALAPPDATA are derived from the target's own profile, never
		// inherited: minimalBase deliberately excludes them, because the
		// daemon's values point into SYSTEM's profile and every DCC writes
		// preferences, license caches, and crash dumps through them.
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

	// Write the OpenJD path-mapping file once, here at session creation, so that
	// BOTH environment actions (entered below) and the task action (run later by
	// the executor) can rely on Session.PathMappingRulesFile pointing at a real
	// file. The executor no longer writes it, avoiding a double-write.
	// Write the OpenJD path-mapping file only when the translation_file delivery
	// is enabled (or, for legacy assignments with no delivery set, fall back to
	// "write if rules exist" to preserve prior behavior).
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
		dynamicEnv:     make(map[string]string),
		dynamicUnset:   make(map[string]bool),
		redactedValues: make(map[string]bool),
		cred:           cred,
		baseEnv:        baseEnv,
	}

	// Enter environments in declaration order.
	if err := s.enterEnvironments(ctx, msg.Environments, m.logger); err != nil {
		// enterEnvironments already ran reverse teardown on already-entered
		// environments. Remove the working directory so the caller gets a clean
		// nil, err return and does not need to call Cleanup on failure. The
		// session (and its s.cred) is discarded along with it — this is the
		// last error path in Create that already obtained a credential, so
		// close it here before returning.
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

	// The session's run-as-user credential (if any) is done being used the
	// moment the session ends — this is the credential's normal-path lifecycle
	// owner, mirroring the error-path closeCredential calls in Manager.Create.
	// Closed unconditionally here (even when keepFailedSessions retains the
	// WORKING DIRECTORY below) since retaining files on disk for inspection
	// has nothing to do with whether the in-memory/OS credential handle is
	// still needed.
	closeCredential(ctx, s.cred, s.ID, m.logger)

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
	if err := writeEmbeddedFiles(s.WorkDir, resolvedFiles, s.cred); err != nil {
		return fmt.Errorf("write embedded files: %w", err)
	}

	if resolvedEnter != nil {
		logger.InfoContext(
			ctx, "session: entering environment",
			slog.String("session_id", s.ID),
			slog.String("env", env.Name),
		)
		if err := runAction(ctx, resolvedEnter, s.WorkDir, s.buildActionEnv(resolvedVars, nil), s.cred, logger, s.actionLineHandler(ctx, logger)); err != nil {
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
// under, or nil when the assignment carried no run_as_user isolation — the
// pre-isolation default, in which every action runs as the worker daemon.
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
// over that, then every key in unset — merged with any dynamic unsets —
// removed. This is the single place session actions (onEnter/onExit)
// construct a command environment, so BaseEnv's filtering is applied exactly
// once, at session creation, and every subsequent layer is job data that is
// never re-filtered.
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
// The process runs under cred (via isolation.Apply; a nil cred is the
// pre-isolation no-op) with env as its complete process environment, already
// built by the caller (see Session.buildActionEnv). workDir is the process
// working directory.
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
// secret values so they never reach the logger. lineHandler may be nil, in
// which case lines are logged verbatim.
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
	// This is job-supplied code (an OpenJD environment onEnter/onExit action):
	// it MUST run under the session's credential, not the daemon's. onEnter in
	// particular runs before any task, so a missed credential here would run
	// arbitrary job code privileged while every task around it looked isolated.
	if err := applyCredential(cmd, cred); err != nil {
		return fmt.Errorf("apply run-as-user credential: %w", err)
	}
	cmd.Env = env

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
// The on-disk filename is computed by [fmtres.EmbeddedFileName] — the same
// helper that builds the Task.File.* / Env.File.* path variables — so the file
// is written exactly where those variables point. Filenames that contain path
// separators or null bytes are rejected to prevent directory traversal.
//
// When cred is non-nil the file is chowned to cred's identity as part of the
// write. Without this, an isolated task is neither the file's owner nor a
// member of its group and there are no "other" bits (0640, or 0750 when
// Runnable) — it can read neither a data file nor execute a runnable one, no
// matter how the surrounding directories are secured. The permission bits
// themselves are deliberately left narrow (never widened to world-readable):
// these files can carry job data, and chowning to the target identity is
// enough to make them readable/executable by exactly the one identity that
// needs them.
//
// Step embedded files are written AFTER job code has already run as the
// target uid (the environment onEnter action runs before any task's embedded
// files are written), at a deterministic name — exactly the shape a
// hardlink/symlink swap at this path could exploit between a naive write and
// a later path-based chown. isolation.WriteFileFchown closes that: it removes
// any existing entry (never following it) and recreates fresh with
// O_EXCL|O_NOFOLLOW, then chowns via fchown on the open descriptor, which
// acts on the inode rather than the path and so cannot be raced. This also
// means a SECOND embedded file legitimately sharing the same name — two
// environments (or an environment and the step) each declaring one named
// "run" — overwrites the first (last-wins, matching fmtres.AddFileVars's
// documented scope semantics) rather than hard-failing the task; only an
// attacker-planted entry is ever defeated, never a legitimate duplicate
// name. See WriteFileFchown's own doc for the full threat model. It is
// itself a no-op chown when cred is nil.
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
// set. A nil/empty set (legacy assignments predating path deliveries) is treated
// as enabled so the OpenJD path-mapping file is still written.
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
