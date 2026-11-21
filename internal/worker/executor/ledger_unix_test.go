// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/proctree"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ledgerRun runs script as a task through execProcess with a session manager
// recording into ledgerDir, in the background. It returns a channel that
// yields the result, the lines the task wrote, and the log output.
func ledgerRun(t *testing.T, ledgerDir string, removeDir bool, script string) (<-chan processResult, *recordingHandler, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	l, err := proctree.OpenLedger(ledgerDir)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if removeDir {
		if err := os.RemoveAll(ledgerDir); err != nil {
			t.Fatal(err)
		}
	}
	mgr := session.NewManager(filepath.Join(newTraversableSessionDataDir(t), "sessions"), false,
		isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger,
		session.WithLedger(l))
	msg := &protocol.AssignMsg{JobID: "job-1", TaskID: "task-1", AttemptID: "attempt-1"}
	sess, err := mgr.Create(context.Background(), msg)
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	t.Cleanup(func() { mgr.Cleanup(context.Background(), sess, false) })
	e := New(status.New(discardNATS{}, status.Config{WorkerID: "test-worker"}, logger), mgr, metrics.New(), DiscardOutput{}, Config{}, logger)
	run := &taskRun{taskID: msg.TaskID, attemptID: msg.AttemptID, sessionID: sess.ID, jobID: msg.JobID}
	out := &recordingHandler{}
	done := make(chan processResult, 1)
	go func() {
		done <- e.execProcess(context.Background(), msg, sess, run, nil,
			&protocol.Action{Command: "sh", Args: []string{"-c", script}}, nil, out)
	}()
	return done, out, logs
}

// awaitResult waits for a background execProcess.
func awaitResult(t *testing.T, done <-chan processResult) processResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("task did not finish within 30s")
		return processResult{}
	}
}

func TestExecProcess_RecordsTreeWhileRunning(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proctrees")
	done, out, _ := ledgerRun(t, dir, false, `echo "tok=$SQI_PROCESS_TREE"; sleep 2`)

	var files []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(files) == 0; time.Sleep(20 * time.Millisecond) {
		files, _ = filepath.Glob(filepath.Join(dir, "*.json")) //nolint:errcheck // the pattern is constant and valid
	}
	if len(files) != 1 {
		t.Fatalf("records while the task runs = %v, want exactly one", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Token string `json:"token"`
		Label struct {
			Kind      string `json:"kind"`
			AttemptID string `json:"attempt_id"`
			TaskID    string `json:"task_id"`
		} `json:"label"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	if rec.Label.Kind != "task" || rec.Label.AttemptID != "attempt-1" || rec.Label.TaskID != "task-1" {
		t.Errorf("record label = %+v, want kind task, attempt-1, task-1", rec.Label)
	}

	if r := awaitResult(t, done); r.Err != nil || r.ExitCode != 0 {
		t.Fatalf("task result = %+v, want a clean exit", r)
	}
	var got string
	for _, l := range out.lines {
		if v, ok := strings.CutPrefix(l, "tok="); ok {
			got = v
		}
	}
	if got == "" || got != rec.Token {
		t.Errorf("task saw SQI_PROCESS_TREE=%q, record token %q; want them equal and non-empty", got, rec.Token)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 { //nolint:errcheck // the pattern is constant and valid
		t.Errorf("records after the task ended: %v", left)
	}
}

func TestExecProcess_RecordFailureStillRuns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proctrees")
	done, _, logs := ledgerRun(t, dir, true, "exit 0")
	if r := awaitResult(t, done); r.Err != nil || r.ExitCode != 0 {
		t.Fatalf("task result = %+v, want a clean exit despite the unwritable ledger", r)
	}
	if !strings.Contains(logs.String(), "executor: record process tree; it will not be stopped if this worker is killed") {
		t.Errorf("log lacks the record-failure warning:\n%s", logs.String())
	}
}
