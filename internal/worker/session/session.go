// SPDX-License-Identifier: AGPL-3.0-only

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
// (sqi.md §7.4, consistent with the session_id column on task_attempts from
// server task 26).
//
// # Phase 1 scope
//
// In Phase 1, sessions are not stored as database rows — they are worker-side
// runtime constructs. The server records the session ID only on task_attempts.
// A dedicated sessions table (for session-reuse scheduling) is deferred to
// Phase 2 per sqi.md §7.4.
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
	"context"
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
	// the session so the server can group attempts by session (task 48).
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

	mu          sync.Mutex
	activeTasks []string                     // task IDs currently executing; managed via AddTask / RemoveTask
	enteredEnvs []protocol.AssignEnvironment // environments entered in declaration order; nil after ExitEnvironments
}

// ActiveTaskCount returns the number of tasks currently executing within this
// session. Safe for concurrent use.
func (s *Session) ActiveTaskCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.activeTasks)
}

// AddTask records taskID as executing within this session.
// Called by the executor when a task process is launched (tasks 49+).
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
// session outcome (task 46).
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
		logger.InfoContext(
			ctx, "session: exiting environment",
			slog.String("session_id", s.ID),
			slog.String("env", env.Name),
		)
		if err := runAction(ctx, env.OnExit, s.WorkDir, env.Variables, logger); err != nil {
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
// for failed sessions are retained for post-mortem inspection (task 47).
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
// declaration order (task 44).
//
// If any OnEnter action fails, already-entered environments are exited in
// reverse order (task 45) and the working directory is removed before the
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

	s := &Session{
		ID:        sessionID,
		WorkDir:   workDir,
		JobID:     msg.JobID,
		CreatedAt: time.Now(),
	}

	// Enter environments in declaration order (task 45).
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
// called already) and removes the session's working directory (task 47).
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

	// Task 47: retain the working directory on failure when the debug flag is set.
	if failed && m.keepFailedSessions {
		m.logger.InfoContext(
			ctx, "session: retaining failed session directory for inspection (--keep-failed-sessions is set)",
			slog.String("session_id", s.ID),
			slog.String("work_dir", s.WorkDir),
		)
		return
	}

	// Remove the working directory (task 47).
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
// in reverse order before the error is returned (task 45).
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
	// Write embedded files before OnEnter runs (environment files may be
	// consumed by the setup action, e.g., an activation script).
	if err := writeEmbeddedFiles(s.WorkDir, env.EmbeddedFiles); err != nil {
		return fmt.Errorf("write embedded files: %w", err)
	}

	if env.OnEnter != nil {
		logger.InfoContext(
			ctx, "session: entering environment",
			slog.String("session_id", s.ID),
			slog.String("env", env.Name),
		)
		if err := runAction(ctx, env.OnEnter, s.WorkDir, env.Variables, logger); err != nil {
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

// ── Action execution ──────────────────────────────────────────────────────────

// runAction starts action.Command as a child process and waits for it to exit.
//
// The process inherits the worker's environment with envVars merged on top
// (task 45 — environment variables set for all actions in the session).
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
// A non-zero exit code is returned as an error that includes the exit code.
func runAction(ctx context.Context, action *protocol.Action, workDir string, envVars map[string]string, logger *slog.Logger) error {
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
	cmd.Env = buildEnv(envVars)

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
	// the other to be drained, both sides block indefinitely.
	var (
		stdoutBytes []byte
		stderrBytes []byte
		stdoutErr   error
		stderrErr   error
		readWg      sync.WaitGroup
	)
	readWg.Add(2)
	go func() {
		defer readWg.Done()
		stdoutBytes, stdoutErr = io.ReadAll(stdout)
	}()
	go func() {
		defer readWg.Done()
		stderrBytes, stderrErr = io.ReadAll(stderr)
	}()
	readWg.Wait()

	// Wait must be called after all pipe reads complete (Go docs: "it is
	// incorrect to call Wait before all reads from the pipe have completed").
	waitErr := cmd.Wait()

	if len(stdoutBytes) > 0 {
		logger.DebugContext(ctx,
			"session: env action stdout",
			slog.String("command", action.Command),
			slog.String("output", string(stdoutBytes)),
		)
	}
	if len(stderrBytes) > 0 {
		logger.DebugContext(ctx,
			"session: env action stderr",
			slog.String("command", action.Command),
			slog.String("output", string(stderrBytes)),
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

// buildEnv constructs the process environment as []string ("KEY=VALUE" pairs)
// by starting from os.Environ() and applying envVars as overrides.
// Keys in envVars take precedence over the inherited environment.
// The output order is non-deterministic (map iteration); callers must not
// rely on any specific ordering of environment variables.
func buildEnv(envVars map[string]string) []string {
	base := os.Environ()
	if len(envVars) == 0 {
		return base
	}

	// Index inherited environment for O(n) override application.
	merged := make(map[string]string, len(base)+len(envVars))
	for _, kv := range base {
		k, v := splitEnvPair(kv)
		merged[k] = v
	}
	maps.Copy(merged, envVars)

	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// splitEnvPair splits "KEY=VALUE" into ("KEY", "VALUE").
// If there is no '=' the key is the whole string and the value is empty.
func splitEnvPair(kv string) (key, value string) {
	for i := range len(kv) {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:]
		}
	}
	return kv, ""
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
// Filenames that contain path separators are rejected to prevent directory
// traversal attacks.
func writeEmbeddedFile(workDir string, f protocol.EmbeddedFile) error {
	name := f.Filename
	if name == "" {
		name = f.Name
	}
	// Guard against path traversal: the resolved filename must equal its base.
	if filepath.Base(name) != name {
		return fmt.Errorf("filename %q must not contain path separators", name)
	}
	// Guard against null bytes, which would silently truncate the filename on
	// some platforms (defense-in-depth; the assignment is server-signed).
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("filename %q must not contain null bytes", name)
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
