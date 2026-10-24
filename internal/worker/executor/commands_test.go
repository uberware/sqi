// SPDX-License-Identifier: AGPL-3.0-or-later

package executor_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/commands"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// newMappedExecutor is newTestExecutor with the operator's worker.commands
// table configured.
func newMappedExecutor(t *testing.T, capture *captureOutput, table commands.Table) (*executor.Executor, *stubNATS) {
	t.Helper()
	nc := &stubNATS{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := session.NewManager(
		filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger,
		session.WithCommands(commands.NewResolver(table)),
	)
	statusPub := status.New(nc, status.Config{WorkerID: "test-worker"}, logger)
	return executor.New(statusPub, mgr, metrics.New(), capture, executor.Config{KillGracePeriod: 500 * time.Millisecond}, logger), nc
}

// mappedAssign runs mappedName with one job argument, as a task's onRun. The
// subprocess mode comes from the environment, so whatever executable the
// mapping points at behaves as the "args" child.
func mappedAssign(name string) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-mapped",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun:     &protocol.Action{Command: name, Args: []string{"job-arg"}},
		Environments: []protocol.AssignEnvironment{
			{Name: "test-env", Variables: map[string]string{"SQI_TEST_SUBPROCESS": "args"}},
		},
	}
}

// mappedName is a command name no host has, so the only way a task can run it
// is through worker.commands.
const mappedName = "sqi-test-mapped-command"

func TestExecutor_RunsAMappedCommandWithItsLeadingArgs(t *testing.T) {
	capture := &captureOutput{}
	exec, nc := newMappedExecutor(t, capture, commands.Table{
		mappedName: {Path: testBinary(), Args: []string{"-test.run=^$", "mapped-arg"}},
	})

	if err := exec.Dispatch(context.Background(), mappedAssign(mappedName)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 20*time.Second)

	if got := nc.lastStatus().Status; got != "succeeded" {
		t.Fatalf("last status = %q, want succeeded; the mapped command did not run (%+v)", got, nc.lastStatus())
	}
	var out []string
	for _, l := range capture.all() {
		out = append(out, l.line)
	}
	// The mapping's args come first, the job's after.
	if want := "-test.run=^$|mapped-arg|job-arg"; !strings.Contains(strings.Join(out, "\n"), want) {
		t.Errorf("child output = %q, want a line %q", out, want)
	}
}

// The same assignment with no mapping must fail, which is what makes the test
// above prove the mapping is what started the process.
func TestExecutor_AnUnmappedUnknownCommandFails(t *testing.T) {
	exec, nc := newMappedExecutor(t, &captureOutput{}, nil)

	if err := exec.Dispatch(context.Background(), mappedAssign(mappedName)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 20*time.Second)

	if got := nc.lastStatus().Status; got != "failed" {
		t.Errorf("last status = %q, want failed for a command that exists nowhere", got)
	}
}
