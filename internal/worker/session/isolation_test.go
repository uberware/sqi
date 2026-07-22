// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"os"
	"os/exec"
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
// was invoked while it was installed. Tests scope each installation to a
// single call site (onEnter here; the task-process launch is covered
// separately in internal/worker/executor/run_test.go), so a simple "was it
// called" is enough to prove that site attaches a credential — the isolation
// package's own fakes cannot verify the actual OS identity switch (that needs
// a real OS; see make test-isolation), only that the call happened at all.
type recordingApplier struct {
	mu     sync.Mutex
	called bool
}

func (r *recordingApplier) record() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
}

// calledFor reports whether Apply was invoked. name documents which call site
// the test expects to have triggered it; it is not itself matched against
// anything, since each test installs the seam around exactly one call site.
func (r *recordingApplier) calledFor(_ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called
}

// newTestSessionWithCredential builds a Session whose assignment carries
// run_as_user isolation, resolved against a fake account with the CURRENT
// process's own uid/gid — chowning a file to the uid it already has is
// permitted without root (see isolation.SecureWorkDir), which is what lets
// this test run without privilege.
//
// applied's applyCredential wrapper records the call and returns nil WITHOUT
// delegating to the real isolation.Apply: actually switching the OS identity
// of a launched process is exactly what a fake cannot verify (real uid/gid
// switching needs a real OS — see make test-isolation and isolation.NewFake's
// own doc). This test proves the call site invokes Apply at all, which is
// the class of bug ("onEnter forgot to carry the credential") this guard
// exists to catch.
func newTestSessionWithCredential(t *testing.T, applied *recordingApplier) *Session {
	t.Helper()

	orig := applyCredential
	applyCredential = func(_ *exec.Cmd, _ *isolation.Credential) error {
		applied.record()
		return nil
	}
	t.Cleanup(func() { applyCredential = orig })

	dataDir := t.TempDir()
	account := isolation.FakeAccount{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": account})
	mgr := NewManager(dataDir, false, provider, workerconfig.IsolationConfig{}, nopLogger())

	msg := &protocol.AssignMsg{
		JobID:     "job-iso",
		Isolation: &protocol.IsolationSpec{User: "render"},
	}
	s, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

// newTestSessionWithoutCredential builds a Session for an assignment that
// carries no run_as_user isolation — the pre-isolation default path.
func newTestSessionWithoutCredential(t *testing.T) *Session {
	t.Helper()

	dataDir := t.TempDir()
	mgr := NewManager(dataDir, false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, nopLogger())
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

	if !applied.calledFor("onEnter") {
		t.Fatal("onEnter must run under the session credential, not the daemon")
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

// ── Credential resolution failure ─────────────────────────────────────────────

// TestManagerCreate_CredentialResolutionFailureNeverFallsBackToDaemon proves
// that a bad run-as-user account fails the session outright rather than
// silently running as the daemon — the one fallback that would look isolated
// while running privileged.
func TestManagerCreate_CredentialResolutionFailureNeverFallsBackToDaemon(t *testing.T) {
	dataDir := t.TempDir()
	provider := isolation.NewFake(nil) // no accounts configured
	mgr := NewManager(dataDir, false, provider, workerconfig.IsolationConfig{}, nopLogger())

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
