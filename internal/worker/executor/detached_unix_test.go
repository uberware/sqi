// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/procout"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// runDetachedScript runs script as a task under sh and returns the result,
// the lines the task wrote, and how long it took.
func runDetachedScript(t *testing.T, script string) (processResult, []string, time.Duration) {
	t.Helper()
	return runDetachedAction(t, context.Background(), &protocol.Action{Command: "sh", Args: []string{"-c", script}})
}

// runDetachedAction runs action as a task under ctx and returns the result,
// the lines the task wrote, and how long it took.
func runDetachedAction(t *testing.T, ctx context.Context, action *protocol.Action) (processResult, []string, time.Duration) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	mgr := session.NewManager(filepath.Join(newTraversableSessionDataDir(t), "sessions"), false,
		isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	msg := &protocol.AssignMsg{JobID: "job-1", TaskID: "task-1", AttemptID: "attempt-1"}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	t.Cleanup(func() { mgr.Cleanup(context.Background(), sess, false) })
	e := New(status.New(discardNATS{}, status.Config{WorkerID: "test-worker"}, logger), mgr, metrics.New(), DiscardOutput{}, Config{}, logger)
	run := &taskRun{taskID: msg.TaskID, attemptID: msg.AttemptID, sessionID: sess.ID, jobID: msg.JobID}
	out := &recordingHandler{}

	start := time.Now()
	result := e.execProcess(ctx, msg, sess, run, nil, action, nil, out)
	// execProcess has returned, so every scanner is done with out.
	return result, out.lines, time.Since(start)
}

// TestExecProcess_FinishesWhenADescendantKeepsTheOutputOpen covers a helper
// that leaves the task's process group, inherits its stdout and stderr, and
// outlives it, as Unreal Editor's detached UnrealTraceServer does. The task
// must end soon after its own process exits, report that process's exit
// code, and keep the output written before it exited. perl stands in for the
// helper: it leaves the group with setpgrp, as a daemon does.
func TestExecProcess_FinishesWhenADescendantKeepsTheOutputOpen(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is not on PATH")
	}
	orig := procout.DrainDelay
	procout.DrainDelay = 200 * time.Millisecond
	defer func() { procout.DrainDelay = orig }()

	for _, tc := range []struct {
		name string
		exit int
	}{
		{name: "clean exit", exit: 0},
		{name: "failed exit", exit: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `perl -e 'setpgrp; print "child=$$\n"; sleep 60' & ` +
				"sleep 0.2; echo written before exit; exit " + strconv.Itoa(tc.exit)
			result, lines, elapsed := runDetachedScript(t, script)
			for _, l := range lines {
				if pid, ok := strings.CutPrefix(l, "child="); ok {
					if n, err := strconv.Atoi(pid); err == nil {
						syscall.Kill(n, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup of the test's own helper
					}
				}
			}
			if elapsed > 10*time.Second {
				t.Fatalf("execProcess took %v: it waited for the descendant holding the output, not for the task", elapsed)
			}
			if result.Err != nil {
				t.Fatalf("execProcess: %v", result.Err)
			}
			if result.ExitCode != tc.exit {
				t.Errorf("ExitCode = %d, want %d", result.ExitCode, tc.exit)
			}
			if !strings.Contains(strings.Join(lines, "\n"), "written before exit") {
				t.Errorf("output %q lost the line written before the task exited", lines)
			}
		})
	}
}

// TestExecProcess_WaitsForABackgroundJobInTheTasksGroup: a job a task script
// started in the background and did not wait for stays in the task's process
// group, so the task waits for it and captures what it writes. Only a holder
// that left the group is cut off.
func TestExecProcess_WaitsForABackgroundJobInTheTasksGroup(t *testing.T) {
	orig := procout.DrainDelay
	procout.DrainDelay = 200 * time.Millisecond
	defer func() { procout.DrainDelay = orig }()

	result, lines, _ := runDetachedScript(t, "(sleep 1; echo later) & echo first; exit 0")

	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("execProcess = %+v, want a clean exit", result)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "later") {
		t.Errorf("output %q lost the line the background job wrote after the script exited", lines)
	}
}

// TestExecProcess_StopsWhenADescendantOutsideTheGroupKeepsTheOutputOpen is the
// cancel and timeout case of the same shape: the task is stopped while a
// helper that left its process group holds the output. The kill empties the
// group, so the task must end within the drain delay after that, before the
// helper exits.
func TestExecProcess_StopsWhenADescendantOutsideTheGroupKeepsTheOutputOpen(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is not on PATH")
	}
	orig := procout.DrainDelay
	procout.DrainDelay = 200 * time.Millisecond
	defer func() { procout.DrainDelay = orig }()

	const script = `perl -e 'setpgrp; print "child=$$\n"; sleep 60' & echo started; sleep 60`
	for _, tc := range []struct {
		name    string
		timeout int
		cancel  bool
	}{
		{name: "canceled", cancel: true},
		{name: "timed out", timeout: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				time.AfterFunc(500*time.Millisecond, cancel)
			}
			result, lines, elapsed := runDetachedAction(t, ctx,
				&protocol.Action{Command: "sh", Args: []string{"-c", script}, TimeoutSeconds: tc.timeout})
			for _, l := range lines {
				if pid, ok := strings.CutPrefix(l, "child="); ok {
					if n, err := strconv.Atoi(pid); err == nil {
						syscall.Kill(n, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup of the test's own helper
					}
				}
			}
			if elapsed > 10*time.Second {
				t.Fatalf("execProcess took %v after the stop: it waited for the helper holding the output", elapsed)
			}
			if result.Canceled != tc.cancel || result.TimedOut != !tc.cancel {
				t.Errorf("result = %+v, want Canceled %v and TimedOut %v", result, tc.cancel, !tc.cancel)
			}
			if !strings.Contains(strings.Join(lines, "\n"), "started") {
				t.Errorf("output %q lost the line written before the stop", lines)
			}
		})
	}
}
