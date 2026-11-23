// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Test scaffolding ──────────────────────────────────────────────────────────

// recordingApplier records whether applyCredential (the isolation.Apply seam)
// was invoked while it was installed, and captures both arguments it was
// called with. Tests scope each installation to a single call site (onEnter
// here; the task-process launch is covered separately in
// internal/worker/executor/run_test.go). It captures cmd and cred, not only
// whether it was called, so a call site passing a nil credential fails. The
// isolation package's fakes cannot verify the OS identity switch (that needs
// a real OS; see make test-isolation), but they can verify that a non-nil,
// session-owned credential reached the seam.
type recordingApplier struct {
	mu     sync.Mutex
	called bool
	cmd    *exec.Cmd
	cred   *isolation.Credential
}

func (r *recordingApplier) record(cmd *exec.Cmd, cred *isolation.Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
	r.cmd = cmd
	r.cred = cred
}

// snapshot returns whether Apply was invoked and the cmd/cred it was invoked
// with.
func (r *recordingApplier) snapshot() (called bool, cmd *exec.Cmd, cred *isolation.Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called, r.cmd, r.cred
}

// testUID and testGID return a uid/gid a fake account can be given that
// session creation can chown its workdir to without privilege: normally the
// current process's own uid/gid (chowning a file to the uid it already has
// needs no privilege; see isolation.SecureWorkDir), but a fixed non-zero
// synthetic pair when the current process is root. isolation.CheckNotPrivileged
// refuses a uid-0 account, so os.Getuid() cannot be a fake account's UID when
// tests run as root (e.g. inside a CI container), and root can chown to any
// uid anyway. 65534 is the conventional "nobody" uid/gid on Linux; any fixed
// non-zero value would do.
func testUID() uint32 {
	if os.Getuid() == 0 {
		return 65534
	}
	return uint32(os.Getuid())
}

func testGID() uint32 {
	if os.Getuid() == 0 {
		return 65534
	}
	return uint32(os.Getgid())
}

// testAccountName returns the account name newTestSessionWithCredential's
// fake account is keyed under, and the name a resolved Credential carries.
//
// On POSIX this is an arbitrary label: the fake credential switches only on
// uid/gid (see testUID/testGID), and no POSIX code path resolves a name. On
// Windows, Manager.Create calls isolation.SecureWorkDir (capableOS is true
// there, so it is not skipped), which resolves a Credential's identity by
// name through Win32 LookupSID; an arbitrary name like "render" is not a
// local account and would fail that lookup. Using the current process's own
// account name (from the USERNAME environment variable, always set on
// Windows) needs no privilege, as testUID/testGID do on POSIX: granting an
// ACE to your own SID needs no special privilege.
func testAccountName() string {
	if runtime.GOOS != "windows" {
		return "render"
	}
	if name := os.Getenv("USERNAME"); name != "" {
		return name
	}
	return "render"
}

// newTestSessionWithCredential builds a Session whose assignment carries
// run_as_user isolation, resolved against a fake account whose uid/gid
// session creation can chown its workdir to without privilege (see
// testUID/testGID).
//
// applied's applyCredential wrapper records the call (both arguments) and
// returns nil without delegating to the real isolation.Apply: a fake cannot
// verify switching the OS identity of a launched process (that needs a real
// OS; see make test-isolation and isolation.NewFake's doc). Tests using it
// prove the call site invokes Apply with a non-nil credential, catching an
// onEnter that carries no credential or a nil one.
func newTestSessionWithCredential(t *testing.T, applied *recordingApplier) *Session {
	t.Helper()

	orig := applyCredential
	applyCredential = func(cmd *exec.Cmd, cred *isolation.Credential) error {
		applied.record(cmd, cred)
		return nil
	}
	t.Cleanup(func() { applyCredential = orig })

	dataDir := newTraversableSessionDataDir(t)
	name := testAccountName()
	account := isolation.FakeAccount{UID: testUID(), GID: testGID()}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{name: account})
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:     "job-iso",
		Isolation: &protocol.IsolationSpec{User: name},
	}
	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

// newTraversableSessionDataDir returns a fresh directory directly under /tmp
// (mode 1777 on every POSIX system), not t.TempDir(), which nests its result
// under a 0700 parent the testing package creates, itself usually under the
// OS's per-user private temp root (e.g. macOS's /var/folders/<x>/<y>/T). Some
// sandboxed dev environments refuse to chmod either, whatever the Unix
// ownership (internal/worker/executor's newCredentialFailureSessionRoot does
// the same). Manager.Create validates every ancestor of the session root for
// an isolated assignment (the per-assignment counterpart of cmd/sqi-worker's
// boot-time check); under /tmp that validation always succeeds, because every
// ancestor above the directory this helper creates (/tmp, and /) is
// traversable.
//
// On Windows isolation.ValidateTraversable is a no-op (NTFS access control is
// ACL-based, not permission-bit-based; see its doc), so the check passes
// wherever the directory lives, and t.TempDir() is used.
func newTraversableSessionDataDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("/tmp", "sqi-session-*") //nolint:usetesting // t.TempDir() has a 0700 parent on POSIX; see doc above
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatalf("chmod %q: %v", dir, err)
	}
	return dir
}

// newTestSessionWithoutCredential builds a Session for an assignment that
// carries no run_as_user isolation.
func newTestSessionWithoutCredential(t *testing.T) *Session {
	t.Helper()

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())
	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "job-plain"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

// runEnvironmentAction runs action through the runAction path onEnter/onExit
// use, without the full enterEnvironments/ExitEnvironments lifecycle: enough
// to prove the credential and the filtered base environment reach the process
// launch.
func (s *Session) runEnvironmentAction(ctx context.Context, action *protocol.Action) error {
	return runAction(ctx, action, s.WorkDir, s.buildActionEnv(nil, nil), s.cred, s.ledger, s.ID, nopLogger(), nil)
}

// testOnEnterAction returns a trivial, always-succeeding POSIX action
// standing in for a real environment's onEnter.
func testOnEnterAction() *protocol.Action {
	return &protocol.Action{Command: "true"}
}

// envSliceToMap converts a "KEY=VALUE" environment slice (as built by
// envutil) into a map for assertions.
func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// ── Guard tests: both job-code launch sites must carry the credential ────────

// TestEnvironmentActionsCarryCredential: onEnter is job-supplied code that
// runs before any task. If it ran as the worker while the tasks around it
// were isolated, isolation would appear to work while job code ran
// unisolated, which nothing but an audit of the OS process would show.
func TestEnvironmentActionsCarryCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test launches a POSIX binary directly")
	}

	applied := &recordingApplier{}
	sess := newTestSessionWithCredential(t, applied)

	if err := sess.runEnvironmentAction(context.Background(), testOnEnterAction()); err != nil {
		t.Fatalf("runEnvironmentAction: %v", err)
	}

	called, cmd, cred := applied.snapshot()
	if !called {
		t.Fatal("onEnter must run under the session credential, not the daemon")
	}
	if cmd == nil {
		t.Fatal("applyCredential was called with a nil *exec.Cmd")
	}
	if cred == nil {
		t.Fatal("applyCredential was called with a nil credential — a guard that discarded both arguments would not catch onEnter passing nil")
	}
	if cred != sess.Credential() {
		t.Error("applyCredential was called with a credential other than the session's own")
	}
}

// ── Environment filtering ─────────────────────────────────────────────────────

func TestSessionBaseEnvFilteredOnceAtCreate(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "secret")

	sess := newTestSessionWithCredential(t, &recordingApplier{})

	if _, ok := sess.BaseEnv()["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("daemon secret must not reach the session base environment")
	}
}

// TestJobSuppliedEnvSurvivesFiltering covers the env_passthrough allowlist's
// scope: the allowlist governs only what is inherited from the worker. A
// job's own dynamic openjd_env export is never filtered, however narrow
// isolation.env_passthrough is.
func TestJobSuppliedEnvSurvivesFiltering(t *testing.T) {
	sess := newTestSessionWithCredential(t, &recordingApplier{})
	sess.applyEnvOp(openjd.EnvOp{Kind: openjd.EnvOpSet, Name: "MY_EXPORT", Value: "v"})

	env := envSliceToMap(sess.buildActionEnv(nil, nil))

	if env["MY_EXPORT"] != "v" {
		t.Error("a job's own openjd_env export must never be filtered by the allowlist")
	}
}

func TestNoIsolationConfiguredIsByteForByteUnchanged(t *testing.T) {
	t.Setenv("SOME_INHERITED", "value")
	sess := newTestSessionWithoutCredential(t)

	env := envSliceToMap(sess.buildActionEnv(nil, nil))

	if env["SOME_INHERITED"] != "value" {
		t.Error("with no run_as_user the full daemon environment must still be inherited")
	}
}

// ── Working directory permissions ─────────────────────────────────────────────

func TestManagerCreate_IsolatedWorkDirIsSecured(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown/chmod semantics are POSIX-specific")
	}

	sess := newTestSessionWithCredential(t, &recordingApplier{})

	info, err := os.Stat(sess.WorkDir)
	if err != nil {
		t.Fatalf("stat workdir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("isolated workdir mode = %v; want 0700", info.Mode().Perm())
	}
}

func TestManagerCreate_NonIsolatedWorkDirUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown/chmod semantics are POSIX-specific")
	}

	sess := newTestSessionWithoutCredential(t)

	info, err := os.Stat(sess.WorkDir)
	if err != nil {
		t.Fatalf("stat workdir: %v", err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Errorf("non-isolated workdir mode = %v; want 0750 (unchanged)", info.Mode().Perm())
	}
}

// TestManagerCreate_SessionRootModeDefaultsTo0711 proves NewManager's default
// (no WithSessionRootMode option given) creates a fresh session root at 0711,
// the mode every call site without the option relies on.
func TestManagerCreate_SessionRootModeDefaultsTo0711(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown/chmod semantics are POSIX-specific")
	}

	dataDir := t.TempDir()
	sessionRoot := filepath.Join(dataDir, "sessions")
	mgr := NewManager(sessionRoot, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())
	msg := &protocol.AssignMsg{JobID: "job-default-mode"}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Cleanup(context.Background(), sess, false) })

	if got := statMode(t, sessionRoot); got != 0o711 {
		t.Errorf("session root mode = %o, want 0711 (the default, unchanged by adding WithSessionRootMode)", got)
	}
}

// TestManagerCreate_SessionRootModeHonorsOption proves WithSessionRootMode
// overrides the default: cmd/sqi-worker's effectiveSessionRoot picks 0750 for
// the non-root DataDir fallback (see its doc) through this option.
func TestManagerCreate_SessionRootModeHonorsOption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown/chmod semantics are POSIX-specific")
	}

	dataDir := t.TempDir()
	sessionRoot := filepath.Join(dataDir, "sessions")
	mgr := NewManager(sessionRoot, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger(), WithSessionRootMode(0o750))
	msg := &protocol.AssignMsg{JobID: "job-explicit-mode"}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Cleanup(context.Background(), sess, false) })

	if got := statMode(t, sessionRoot); got != 0o750 {
		t.Errorf("session root mode = %o, want 0750 (WithSessionRootMode override)", got)
	}
}

// statMode returns path's permission bits.
func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return info.Mode().Perm()
}

// ── Per-assignment ancestor validation ────────────────────────────────────────

// TestManagerCreate_IsolatedAssignmentFailsOnNonTraversableSessionRoot is the
// per-assignment guard: when an assignment carries run_as_user isolation,
// Create validates the session root's ancestors (the check, and message,
// cmd/sqi-worker runs at boot only when isolation.required is set) and fails
// only this task (an error return, not a worker crash) rather than letting
// the target uid fail a chdir it cannot make.
func TestManagerCreate_IsolatedAssignmentFailsOnNonTraversableSessionRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	dir := t.TempDir()
	narrow := filepath.Join(dir, "narrow")
	if err := os.Mkdir(narrow, 0o700); err != nil {
		t.Fatalf("create narrow ancestor: %v", err)
	}
	sessionRoot := filepath.Join(narrow, "sessions")

	account := isolation.FakeAccount{UID: testUID(), GID: testGID()}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": account})
	mgr := NewManager(sessionRoot, false, provider, workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{JobID: "job-narrow", Isolation: &protocol.IsolationSpec{User: "render"}}
	_, err := mgr.Create(context.Background(), msg)

	if err == nil {
		t.Fatal("expected an error naming the non-traversable ancestor; got nil")
	}
	if !strings.Contains(err.Error(), narrow) {
		t.Errorf("err = %v, want it to name the offending ancestor %q", err, narrow)
	}
}

// TestManagerCreate_NonIsolatedAssignmentSkipsAncestorValidation proves the
// per-assignment check applies to isolated assignments only: the narrow
// ancestor that fails an isolated Create above must not stop a non-isolated
// one, since no other identity needs to chdir through it.
func TestManagerCreate_NonIsolatedAssignmentSkipsAncestorValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	dir := t.TempDir()
	narrow := filepath.Join(dir, "narrow")
	if err := os.Mkdir(narrow, 0o700); err != nil {
		t.Fatalf("create narrow ancestor: %v", err)
	}
	sessionRoot := filepath.Join(narrow, "sessions")

	mgr := NewManager(sessionRoot, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())
	msg := &protocol.AssignMsg{JobID: "job-narrow-noiso"}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create (no isolation) under a narrow-but-self-owned ancestor: %v", err)
	}
	t.Cleanup(func() { mgr.Cleanup(context.Background(), sess, false) })
}

// ── Credential resolution failure ─────────────────────────────────────────────

// TestManagerCreate_CredentialResolutionFailureNeverFallsBackToDaemon proves
// that a bad run-as-user account fails the session rather than running as
// the worker, a fallback that would look isolated while running privileged.
func TestManagerCreate_CredentialResolutionFailureNeverFallsBackToDaemon(t *testing.T) {
	dataDir := t.TempDir()
	provider := isolation.NewFake(nil) // no accounts configured
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:     "job-bad-account",
		Isolation: &protocol.IsolationSpec{User: "no-such-user"},
	}
	s, err := mgr.Create(context.Background(), msg)
	if err == nil {
		t.Fatal("expected Create to fail for an unresolvable run-as-user account")
	}
	if s != nil {
		t.Errorf("Create must return nil session on credential-resolution failure; got %+v", s)
	}
}
