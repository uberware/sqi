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

	"github.com/uberware/sqi/internal/worker/proctree"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
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
// recording into ledgerDir, in the background; with removeDir, ledgerDir is
// removed once the ledger is open, so recording fails. It returns a channel
// that yields the result, the lines the task wrote, and the log output.
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
	d := newDirectRun(t, logger, session.WithLedger(l))
	out := &recordingHandler{}
	done := make(chan processResult, 1)
	go func() {
		done <- d.e.execProcess(context.Background(), d.msg, d.sess, d.run, nil,
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
		Token string         `json:"token"`
		Label proctree.Label `json:"label"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	if want := (proctree.Label{Kind: "task", AttemptID: "attempt-1", TaskID: "task-1", SessionID: rec.Label.SessionID}); rec.Label != want || rec.Label.SessionID == "" {
		t.Errorf("record label = %+v, want kind task, attempt-1, task-1 and the session", rec.Label)
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
