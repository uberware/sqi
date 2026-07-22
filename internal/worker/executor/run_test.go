// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// recordingApplier records whether applyCredential (the isolation.Apply seam
// in this package) was invoked, and captures both arguments it was called
// with. It intentionally does not delegate to the real isolation.Apply:
// actually switching the OS identity of a launched process is exactly what a
// fake cannot verify — that needs a real OS (see make test-isolation and
// isolation.NewFake's own doc). This test proves execProcess invokes Apply at
// all for the task-process launch site, AND that it is invoked with the
// actual cmd and the session's own (non-nil) credential — not just that some
// call happened, which would still pass a guard that discarded both
// arguments even if a launch site started passing nil.
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
// with, for the caller to assert against directly (e.g. that cred is not nil
// and is the session's own credential).
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

// makeDataDirAncestorsTraversableForTest and
// skipIfEnvironmentBlocksSessionTraversal are this package's copies of the
// identically-named helpers in internal/worker/session's own isolation_test.go
// — see that file's doc for why they exist: since Finding 4,
// session.Manager.Create validates every ancestor of the session root for an
// isolated assignment, so a test dataDir built on plain t.TempDir() needs the
// same traversal-fixture treatment a real, operator-provisioned location
// gets in production.
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

func skipIfEnvironmentBlocksSessionTraversal(t *testing.T, err error, ownedDir string) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), ownedDir) {
		return
	}
	t.Skipf("environment appears to block making a temp-dir ancestor traversable "+
		"(chmod likely restricted by a sandbox regardless of Unix ownership), "+
		"which this fixture cannot work around: %v", err)
}

// discardNATS is a minimal natsPublisher that drops every message; the
// guard test below only cares about process launch, not published status.
type discardNATS struct{}

func (discardNATS) Publish(_ string, _ []byte) error { return nil }

// TestTaskActionsCarryCredential is the guard test for launch site 2 — the
// task's OnRun action, started in execProcess. Launch site 1 (the OpenJD
// environment onEnter/onExit action) is covered by
// TestEnvironmentActionsCarryCredential in
// internal/worker/session/isolation_test.go. Together these two tests prove
// that isolation.Apply is not missed at either job-code launch site.
func TestTaskActionsCarryCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test launches a POSIX binary directly")
	}

	applied := &recordingApplier{}
	orig := applyCredential
	applyCredential = func(cmd *exec.Cmd, cred *isolation.Credential) error {
		applied.record(cmd, cred)
		return nil
	}
	defer func() { applyCredential = orig }()

	logger := slog.New(slog.DiscardHandler)
	dataDir := t.TempDir()
	makeDataDirAncestorsTraversableForTest(t, dataDir)
	// A fake account resolved to a uid/gid session creation can actually
	// chown to without a real privilege escalation: the current process's own
	// (see isolation.SecureWorkDir), unless that current process IS root (uid
	// 0) — in which case CheckNotPrivileged would refuse a uid-0 account
	// outright (and root can chown to any uid regardless), so testUID/testGID
	// substitute a fixed non-zero synthetic uid/gid when running as root.
	uid, gid := testUID(), testGID()
	account := isolation.FakeAccount{UID: uid, GID: gid}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": account})
	mgr := session.NewManager(filepath.Join(dataDir, "sessions"), false, provider, workerconfig.IsolationConfig{}, logger)

	msg := &protocol.AssignMsg{
		JobID:     "job-1",
		TaskID:    "task-1",
		AttemptID: "attempt-1",
		Isolation: &protocol.IsolationSpec{User: "render"},
	}
	sess, err := mgr.Create(context.Background(), msg)
	skipIfEnvironmentBlocksSessionTraversal(t, err, dataDir)
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	defer mgr.Cleanup(context.Background(), sess, false)

	statusPub := status.New(discardNATS{}, status.Config{WorkerID: "test-worker"}, logger)
	e := New(statusPub, mgr, metrics.New(), DiscardOutput{}, Config{}, logger)

	run := &taskRun{taskID: msg.TaskID, attemptID: msg.AttemptID, sessionID: sess.ID, jobID: msg.JobID}
	result := e.execProcess(context.Background(), msg, sess, run, nil, &protocol.Action{Command: "true"}, nil)
	if result.Err != nil {
		t.Fatalf("execProcess: %v", result.Err)
	}

	called, cmd, cred := applied.snapshot()
	if !called {
		t.Fatal("the task's OnRun action must run under the session credential, not the daemon")
	}
	if cmd == nil {
		t.Fatal("applyCredential was called with a nil *exec.Cmd")
	}
	if cred == nil {
		t.Fatal("applyCredential was called with a nil credential — a guard that discarded both arguments would not catch a launch site passing nil")
	}
	if cred != sess.Credential() {
		t.Error("applyCredential was called with a credential other than the session's own")
	}
}
