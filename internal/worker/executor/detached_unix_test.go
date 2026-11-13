// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"context"
	"log/slog"
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

// TestExecProcess_FinishesWhenADescendantKeepsTheOutputOpen reproduces a task
// that never finished: Unreal Editor forks a detached UnrealTraceServer that
// inherits the editor's stdout and stderr and outlives it. The worker read the
// task's output to EOF before it waited for the process, so the editor's exit
// went unseen for as long as the helper lived, and with no timeout that was
// forever. The task must end soon after its own process exits, report that
// process's exit code, and keep the output written before it exited.
func TestExecProcess_FinishesWhenADescendantKeepsTheOutputOpen(t *testing.T) {
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
			logger := slog.New(slog.DiscardHandler)
			mgr := session.NewManager(filepath.Join(newTraversableSessionDataDir(t), "sessions"), false,
				isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
			msg := &protocol.AssignMsg{JobID: "job-1", TaskID: "task-1", AttemptID: "attempt-1"}
			sess, err := mgr.Create(context.Background(), msg)
			if err != nil {
				t.Fatalf("session Create: %v", err)
			}
			defer mgr.Cleanup(context.Background(), sess, false)
			e := New(status.New(discardNATS{}, status.Config{WorkerID: "test-worker"}, logger), mgr, metrics.New(), DiscardOutput{}, Config{}, logger)
			run := &taskRun{taskID: msg.TaskID, attemptID: msg.AttemptID, sessionID: sess.ID, jobID: msg.JobID}
			out := &recordingHandler{}

			// The sleep inherits the shell's stdout and stderr and outlives it.
			script := "sleep 60 & echo child=$!; echo written before exit; exit " + strconv.Itoa(tc.exit)
			start := time.Now()
			result := e.execProcess(context.Background(), msg, sess, run, nil,
				&protocol.Action{Command: "sh", Args: []string{"-c", script}}, nil, out)
			elapsed := time.Since(start)

			// execProcess has returned, so every scanner is done with out.
			lines := out.lines
			for _, l := range lines {
				if pid, ok := strings.CutPrefix(l, "child="); ok {
					if n, err := strconv.Atoi(pid); err == nil {
						syscall.Kill(n, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup of the test's own sleep
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
