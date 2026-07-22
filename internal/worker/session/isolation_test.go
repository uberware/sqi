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
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Test scaffolding ──────────────────────────────────────────────────────────

// recordingApplier records whether applyCredential (the isolation.Apply seam)
// was invoked while it was installed, and captures both arguments it was
// called with. Tests scope each installation to a single call site (onEnter
// here; the task-process launch is covered separately in
// internal/worker/executor/run_test.go). Capturing cmd and cred (rather than
// just a called bool) matters: a guard that only recorded "was it called"
// would still pass unchanged if the call site started passing a nil
// credential — the isolation package's own fakes cannot verify the actual OS
// identity switch (that needs a real OS; see make test-isolation), but they
// CAN verify that a real, non-nil, session-owned credential reached the seam.
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
// session creation can actually chown its workdir to without real privilege:
// normally the current process's own uid/gid (chowning a file to the uid it
// already has needs no privilege — see isolation.SecureWorkDir), but a fixed
// non-zero synthetic pair when the current process IS root. Two reasons the
// root case needs its own value: isolation.CheckNotPrivileged unconditionally
// refuses a uid-0 account (so os.Getuid() itself cannot be used as a fake
// account's UID when tests run as root, e.g. inside a container in CI — a
// routine setup, not a hypothetical one), and root can chown to ANY uid
// regardless, so there is no privilege reason to prefer the process's own.
// 65534 is the conventional "nobody" uid/gid on Linux — any fixed non-zero
// value would do.
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

// newTestSessionWithCredential builds a Session whose assignment carries
// run_as_user isolation, resolved against a fake account whose uid/gid
// session creation can actually chown its workdir to without real privilege
// (see testUID/testGID) — which is what lets this test run without privilege.
//
// applied's applyCredential wrapper records the call (both arguments) and
// returns nil WITHOUT delegating to the real isolation.Apply: actually
// switching the OS identity of a launched process is exactly what a fake
// cannot verify (real uid/gid switching needs a real OS — see
// make test-isolation and isolation.NewFake's own doc). This test proves the
// call site invokes Apply with a real, non-nil credential, which is the class
// of bug ("onEnter forgot to carry the credential", or carried a nil one)
// this guard exists to catch.
func newTestSessionWithCredential(t *testing.T, applied *recordingApplier) *Session {
	t.Helper()

	orig := applyCredential
	applyCredential = func(cmd *exec.Cmd, cred *isolation.Credential) error {
		applied.record(cmd, cred)
		return nil
	}
	t.Cleanup(func() { applyCredential = orig })

	dataDir := t.TempDir()
	makeDataDirAncestorsTraversableForTest(t, dataDir)
	account := isolation.FakeAccount{UID: testUID(), GID: testGID()}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": account})
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:     "job-iso",
		Isolation: &protocol.IsolationSpec{User: "render"},
	}
	s, err := mgr.Create(context.Background(), msg)
	skipIfEnvironmentBlocksSessionTraversal(t, err, dataDir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

// makeDataDirAncestorsTraversableForTest is this package's analog of
// internal/worker/isolation/workdir_unix_test.go's own
// makeAncestorsTraversableForTest: since Finding 4, Manager.Create validates
// every ancestor of the session root for an isolated assignment (the
// per-assignment counterpart of cmd/sqi-worker's boot-time check), so a test
// dataDir built on plain t.TempDir() now needs the same traversal-fixture
// treatment production callers get from a real, operator-provisioned
// location. t.TempDir()'s own return value sits under a testing-package-owned
// parent (created via os.MkdirTemp, hardcoded 0700 regardless of umask) that
// this test process DOES own and can chmod, itself typically sitting under
// the OS's own per-user private temp root (e.g. macOS's
// /var/folders/<x>/<y>/T), which some sandboxed dev environments refuse to
// chmod regardless of Unix ownership — see
// skipIfEnvironmentBlocksSessionTraversal for how that residual case is
// handled. Capped at a handful of levels for the same reason as the
// isolation package's own analog.
func makeDataDirAncestorsTraversableForTest(t *testing.T, dir string) {
	t.Helper()
	const maxLevels = 4
	for range maxLevels {
		if err := os.Chmod(dir, 0o711); err != nil {
			return // reached a directory this test process does not own; stop climbing
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return // reached the filesystem root
		}
		dir = parent
	}
}

// skipIfEnvironmentBlocksSessionTraversal is this package's analog of
// isolation's own skipIfEnvironmentBlocksTraversalFixture: it reports (via
// t.Skip) only when err names a directory OUTSIDE ownedDir's own tree — an
// OS-imposed ancestor makeDataDirAncestorsTraversableForTest could not fix
// regardless of ownership — leaving a failure INSIDE ownedDir (a real defect
// in the code under test) to the caller's own error handling untouched.
func skipIfEnvironmentBlocksSessionTraversal(t *testing.T, err error, ownedDir string) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), ownedDir) {
		return
	}
	t.Skipf("environment appears to block making a temp-dir ancestor traversable "+
		"(chmod likely restricted by a sandbox regardless of Unix ownership), "+
		"which this fixture cannot work around: %v", err)
}

// newTestSessionWithoutCredential builds a Session for an assignment that
// carries no run_as_user isolation — the pre-isolation default path.
func newTestSessionWithoutCredential(t *testing.T) *Session {
	t.Helper()

	dataDir := t.TempDir()
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())
	s, err := mgr.Create(context.Background(), &protocol.AssignMsg{JobID: "job-plain"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

// runEnvironmentAction runs action through the exact runAction path
// onEnter/onExit use, without driving the full enterEnvironments/
// ExitEnvironments lifecycle — enough to prove the credential and the
// filtered base environment reach the process launch.
func (s *Session) runEnvironmentAction(ctx context.Context, action *protocol.Action) error {
	return runAction(ctx, action, s.WorkDir, s.buildActionEnv(nil, nil), s.cred, nopLogger(), nil)
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

// TestEnvironmentActionsCarryCredential is the guard test for the worst
// available bug in this feature. onEnter is job-supplied code that runs
// BEFORE any task. If it executed as the daemon while tasks around it were
// correctly isolated, isolation would look like it worked while leaving the
// hole wide open — invisible in the web UI, in status messages, everywhere
// except an audit of the actual OS process.
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

// TestJobSuppliedEnvSurvivesFiltering is the regression guard for the
// audit's core finding, expressed end to end: the allowlist governs ONLY what
// is inherited from the daemon. A job's own dynamic openjd_env export must
// never be filtered, no matter how narrow isolation.env_passthrough is.
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
// (no WithSessionRootMode option given) still creates a fresh session root at
// 0711 — the mode every call site predating WithSessionRootMode relies on, so
// this option's addition must not silently change behavior for any of them.
func TestManagerCreate_SessionRootModeDefaultsTo0711(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown/chmod semantics are POSIX-specific")
	}

	dataDir := t.TempDir()
	sessionRoot := filepath.Join(dataDir, "sessions")
	mgr := NewManager(sessionRoot, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())
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
// the non-root DataDir fallback (the pre-split mode, restored for
// byte-for-byte backward compatibility — see its own doc), and this is the
// mechanism that lets it do so without touching every other NewManager call
// site in this codebase.
func TestManagerCreate_SessionRootModeHonorsOption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chown/chmod semantics are POSIX-specific")
	}

	dataDir := t.TempDir()
	sessionRoot := filepath.Join(dataDir, "sessions")
	mgr := NewManager(sessionRoot, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger(), WithSessionRootMode(0o750))
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

// ── Per-assignment ancestor validation (Finding 4) ────────────────────────────

// TestManagerCreate_IsolatedAssignmentFailsOnNonTraversableSessionRoot is the
// per-assignment guard: when an assignment actually carries run_as_user
// isolation, Create validates the session root's ancestors — the IDENTICAL
// check (same actionable message) cmd/sqi-worker runs at boot only when
// isolation.required is set — and fails just THIS task (a plain error
// return, never a worker crash) rather than letting the target uid fail a
// chdir it cannot make.
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
	mgr := NewManager(sessionRoot, false, provider, workerconfig.IsolationConfig{}, nopLogger())

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
// per-assignment check is scoped to isolated assignments only: the exact
// same narrow ancestor that fails an isolated Create above must not stop a
// non-isolated one — no other identity will ever need to chdir through it.
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

	mgr := NewManager(sessionRoot, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())
	msg := &protocol.AssignMsg{JobID: "job-narrow-noiso"}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create (no isolation) under a narrow-but-self-owned ancestor: %v", err)
	}
	t.Cleanup(func() { mgr.Cleanup(context.Background(), sess, false) })
}

// ── Credential resolution failure ─────────────────────────────────────────────

// TestManagerCreate_CredentialResolutionFailureNeverFallsBackToDaemon proves
// that a bad run-as-user account fails the session outright rather than
// silently running as the daemon — the one fallback that would look isolated
// while running privileged.
func TestManagerCreate_CredentialResolutionFailureNeverFallsBackToDaemon(t *testing.T) {
	dataDir := t.TempDir()
	provider := isolation.NewFake(nil) // no accounts configured
	mgr := NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, nopLogger())

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
