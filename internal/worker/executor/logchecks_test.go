// SPDX-License-Identifier: AGPL-3.0-or-later

package executor_test

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// linesAssign builds a "lines"-mode assignment (see TestMain) printing output
// and carrying the given log checks.
func linesAssign(output string, extraEnv map[string]string, checks *protocol.LogChecks) *protocol.AssignMsg {
	env := map[string]string{"SQI_TEST_OUTPUT": output}
	maps.Copy(env, extraEnv)
	msg := makeAssign("lines", env)
	msg.LogChecks = checks
	return msg
}

// runChecked dispatches msg and returns the terminal status.
func runChecked(t *testing.T, msg *protocol.AssignMsg) protocol.TaskStatusMsg {
	t.Helper()
	exec, nc, _ := newTestExecutor(t, nil)
	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)
	return nc.lastStatus()
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}
}

func TestExecutor_LogChecks_FailOnStdout(t *testing.T) {
	skipOnWindows(t)
	last := runChecked(t, linesAssign("working|ERROR boom|done", nil,
		&protocol.LogChecks{FailOn: []string{"ERROR"}}))
	if last.Status != "failed" {
		t.Fatalf("status = %q (%q), want failed", last.Status, last.Message)
	}
	want := `log line matched failOn pattern "ERROR": "ERROR boom"`
	if last.Message != want {
		t.Errorf("message = %q, want %q", last.Message, want)
	}
}

func TestExecutor_LogChecks_FailOnStderr(t *testing.T) {
	skipOnWindows(t)
	last := runChecked(t, linesAssign("working|err:ERROR boom", nil,
		&protocol.LogChecks{FailOn: []string{"ERROR"}}))
	if last.Status != "failed" {
		t.Fatalf("status = %q (%q), want failed", last.Status, last.Message)
	}
	if !strings.HasPrefix(last.Message, "log line matched failOn pattern") {
		t.Errorf("message = %q", last.Message)
	}
}

func TestExecutor_LogChecks_RequireLine(t *testing.T) {
	skipOnWindows(t)
	checks := &protocol.LogChecks{RequireLine: []string{"SQI_RENDER: OK"}}

	missing := runChecked(t, linesAssign("working|done", nil, checks))
	if missing.Status != "failed" {
		t.Fatalf("missing: status = %q, want failed", missing.Status)
	}
	want := `the task never printed a line matching requireLine pattern "SQI_RENDER: OK"`
	if missing.Message != want {
		t.Errorf("missing: message = %q, want %q", missing.Message, want)
	}

	// The marker behind a host log prefix still satisfies it.
	present := runChecked(t, linesAssign(
		"working|[2026.10.28-12.00.00:000][  0]LogPython: SQI_RENDER: OK", nil, checks,
	))
	if present.Status != "succeeded" {
		t.Errorf("present: status = %q (%q), want succeeded", present.Status, present.Message)
	}
}

func TestExecutor_LogChecks_RequireLineOnStderr(t *testing.T) {
	skipOnWindows(t)
	last := runChecked(t, linesAssign("err:SQI_RENDER: OK", nil,
		&protocol.LogChecks{RequireLine: []string{"SQI_RENDER: OK"}}))
	if last.Status != "succeeded" {
		t.Errorf("status = %q (%q), want succeeded", last.Status, last.Message)
	}
}

// A non-zero exit wins over a failOn match, and over everything else.
func TestExecutor_LogChecks_ExitStatusWins(t *testing.T) {
	skipOnWindows(t)
	last := runChecked(t, linesAssign("ERROR boom", map[string]string{"SQI_TEST_EXIT_CODE": "3"},
		&protocol.LogChecks{FailOn: []string{"ERROR"}}))
	if last.Status != "failed" || last.Message != "process exited with code 3" {
		t.Errorf("status = %q, message = %q; want failed / process exited with code 3", last.Status, last.Message)
	}
	if last.ExitCode == nil || *last.ExitCode != 3 {
		t.Errorf("ExitCode = %v, want 3", last.ExitCode)
	}
}

// Checks do not override cancel or timeout.
func TestExecutor_LogChecks_DoNotOverrideCancel(t *testing.T) {
	skipOnWindows(t)
	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := linesAssign("ERROR boom", map[string]string{"SQI_TEST_SLEEP": "30s"},
		&protocol.LogChecks{FailOn: []string{"ERROR"}, RequireLine: []string{"never printed"}})
	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForOutputLine(t, capture, "ERROR boom", 10*time.Second)
	if !exec.Cancel(msg.TaskID) {
		t.Fatal("Cancel returned false")
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "canceled" {
		t.Errorf("status = %q (%q), want canceled", last.Status, last.Message)
	}
}

func TestExecutor_LogChecks_DoNotOverrideTimeout(t *testing.T) {
	skipOnWindows(t)
	msg := linesAssign("ERROR boom", map[string]string{"SQI_TEST_SLEEP": "30s"},
		&protocol.LogChecks{FailOn: []string{"ERROR"}})
	msg.OnRun.TimeoutSeconds = 1

	last := runChecked(t, msg)
	if last.Status != "failed" || !strings.HasPrefix(last.Message, "execution timeout after") {
		t.Errorf("status = %q, message = %q; want failed with the timeout reason", last.Status, last.Message)
	}
}

// A redacted value echoed on a failOn line must reach the failure reason as the
// placeholder, never as the secret.
func TestExecutor_LogChecks_ReasonIsRedacted(t *testing.T) {
	skipOnWindows(t)
	msg := linesAssign("ERROR token=shh", nil, &protocol.LogChecks{FailOn: []string{"ERROR"}})
	msg.Environments = append([]protocol.AssignEnvironment{{
		Name: "exporter",
		OnEnter: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo 'openjd_redacted_env: SECRET=shh'"},
		},
	}}, msg.Environments...)

	last := runChecked(t, msg)
	if last.Status != "failed" {
		t.Fatalf("status = %q (%q), want failed", last.Status, last.Message)
	}
	if strings.Contains(last.Message, "shh") {
		t.Errorf("secret leaked into the failure reason: %q", last.Message)
	}
	if !strings.Contains(last.Message, "<redacted>") {
		t.Errorf("reason %q does not carry the redaction placeholder", last.Message)
	}
}

// Without LogChecks, statuses follow the exit code alone.
func TestExecutor_LogChecks_AbsentChangesNothing(t *testing.T) {
	skipOnWindows(t)
	ok := runChecked(t, makeAssign("stdout", map[string]string{"SQI_TEST_OUTPUT": "ERROR looks bad but nobody checks"}))
	if ok.Status != "succeeded" {
		t.Errorf("stdout: status = %q, want succeeded", ok.Status)
	}
	bad := runChecked(t, makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "42"}))
	if bad.Status != "failed" || bad.ExitCode == nil || *bad.ExitCode != 42 {
		t.Errorf("exit: status = %q, ExitCode = %v, want failed / 42", bad.Status, bad.ExitCode)
	}
}

// A pattern that does not compile (the server validated it, so this is
// defense) fails the task before it runs.
func TestExecutor_LogChecks_InvalidPatternFailsPreExec(t *testing.T) {
	skipOnWindows(t)
	last := runChecked(t, linesAssign("hello", nil, &protocol.LogChecks{FailOn: []string{"(unclosed"}}))
	if last.Status != "failed" {
		t.Fatalf("status = %q, want failed", last.Status)
	}
	if !strings.HasPrefix(last.Message, "invalid SQI_LOG_CHECKS pattern: ") {
		t.Errorf("message = %q, want the invalid-pattern prefix", last.Message)
	}
}

// A failed verdict must not stage outputs: the OUT entry below has no file, so
// reaching stage-out would replace the check's reason with a staging error.
func TestExecutor_LogChecks_VerdictFailureDoesNotStageOut(t *testing.T) {
	skipOnWindows(t)

	run := func(checks *protocol.LogChecks) protocol.TaskStatusMsg {
		nc := &stubNATS{}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		mgr := session.NewManager(filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
		cfg := executor.Config{KillGracePeriod: 500 * time.Millisecond, StagingDefaults: true}
		exec := executor.New(status.New(nc, status.Config{WorkerID: "test-worker"}, logger), mgr, metrics.New(), nil, cfg, logger)

		msg := linesAssign("ERROR boom", nil, checks)
		msg.PathDeliveries = []protocol.PathDelivery{{Kind: "stage_locally"}}
		msg.Staging = []protocol.StageEntry{{Path: filepath.Join(t.TempDir(), "out", "frame.exr"), Direction: "OUT"}}
		if err := exec.Dispatch(context.Background(), msg); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		waitForStatus(t, nc, 2, 10*time.Second)
		return nc.lastStatus()
	}

	// Control: with no check the task reaches stage-out, which fails.
	control := run(nil)
	if control.Status != "failed" || !strings.Contains(control.Message, "staging") {
		t.Fatalf("control: status = %q, message = %q; want a stage-out failure",
			control.Status, control.Message)
	}

	checked := run(&protocol.LogChecks{FailOn: []string{"ERROR"}})
	if checked.Status != "failed" || !strings.HasPrefix(checked.Message, "log line matched failOn pattern") {
		t.Errorf("status = %q, message = %q; want the failOn reason, not a staging error", checked.Status, checked.Message)
	}
}
