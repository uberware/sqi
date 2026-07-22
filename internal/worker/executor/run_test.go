// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
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
// in this package) was invoked. It intentionally does not delegate to the
// real isolation.Apply: actually switching the OS identity of a launched
// process is exactly what a fake cannot verify — that needs a real OS (see
// make test-isolation and isolation.NewFake's own doc). This test proves
// execProcess invokes Apply at all for the task-process launch site, which is
// the class of bug this guard exists to catch.
type recordingApplier struct {
	mu     sync.Mutex
	called bool
}

func (r *recordingApplier) record() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
}

func (r *recordingApplier) calledFor(_ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called
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
	applyCredential = func(_ *exec.Cmd, _ *isolation.Credential) error {
		applied.record()
		return nil
	}
	defer func() { applyCredential = orig }()

	logger := slog.New(slog.DiscardHandler)
	dataDir := t.TempDir()
	// A fake account resolved to the CURRENT process's own uid/gid: chowning
	// a file to the uid it already has is permitted without root (see
	// isolation.SecureWorkDir), which is what lets session creation succeed
	// here without privilege.
	account := isolation.FakeAccount{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	provider := isolation.NewFake(map[string]isolation.FakeAccount{"render": account})
	mgr := session.NewManager(dataDir, false, provider, workerconfig.IsolationConfig{}, logger)

	msg := &protocol.AssignMsg{
		JobID:     "job-1",
		TaskID:    "task-1",
		AttemptID: "attempt-1",
		Isolation: &protocol.IsolationSpec{User: "render"},
	}
	sess, err := mgr.Create(context.Background(), msg)
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

	if !applied.calledFor("task") {
		t.Fatal("the task's OnRun action must run under the session credential, not the daemon")
	}
}
