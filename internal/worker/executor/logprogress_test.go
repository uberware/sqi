// SPDX-License-Identifier: AGPL-3.0-or-later

package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/openjd"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

var _ executor.ProgressPatternHook = (*openjd.Interceptor)(nil)

// lockedBuffer is a bytes.Buffer safe for concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runWithInterceptor dispatches msg on an executor whose output handler is the
// real OpenJD interceptor (unthrottled), and returns every status message
// published, in order, and the worker's log output.
func runWithInterceptor(t *testing.T, msg *protocol.AssignMsg) ([]protocol.TaskStatusMsg, string) {
	t.Helper()
	tmpDir := t.TempDir()
	nc := &stubNATS{}
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := session.NewManager(filepath.Join(tmpDir, "sessions"), false, isolation.NewFake(nil),
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	statusPub := status.New(nc, status.Config{WorkerID: "test-worker"}, logger)
	ic := openjd.New(executor.DiscardOutput{}, nc, "test-worker", logger, openjd.WithProgressInterval(0))
	exec := executor.New(statusPub, mgr, metrics.New(), ic, executor.Config{KillGracePeriod: 500 * time.Millisecond}, logger)

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		msgs := decodeAll(t, nc)
		if n := len(msgs); n > 0 && msgs[n-1].Status != "running" {
			return msgs, logs.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a terminal status; got %+v", msgs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func decodeAll(t *testing.T, nc *stubNATS) []protocol.TaskStatusMsg {
	t.Helper()
	nc.mu.Lock()
	defer nc.mu.Unlock()
	var out []protocol.TaskStatusMsg
	for _, m := range nc.msgs {
		var msg protocol.TaskStatusMsg
		if err := json.Unmarshal(m.data, &msg); err == nil && msg.Type == protocol.TypeTaskStatus {
			out = append(out, msg)
		}
	}
	return out
}

func progressAssign(output string, lp *protocol.LogProgress) *protocol.AssignMsg {
	msg := linesAssign(output, nil, nil)
	msg.LogProgress = lp
	return msg
}

func TestExecutor_LogProgressSetsProgressAndStatus(t *testing.T) {
	skipOnWindows(t)
	msg := progressAssign("Fra:1|Fra:2|Fra:3|Fra:4|err:phase: compositing", &protocol.LogProgress{
		FrameParameter: "Frame",
		Progress:       []string{`^Fra:(?P<frame>\d+)$`},
		Status:         []string{`^phase: (?P<status>.+)$`},
	})
	msg.Parameters = map[string]string{"Frame": "1-4"}

	msgs, _ := runWithInterceptor(t, msg)
	last := msgs[len(msgs)-1]
	if last.Status != "succeeded" {
		t.Fatalf("status = %q (%q), want succeeded", last.Status, last.Message)
	}
	if last.LastProgress == nil || *last.LastProgress != 75 {
		t.Errorf("terminal LastProgress = %v, want 75 (frame 4 of 4 started)", last.LastProgress)
	}
	sawStatus := false
	for _, m := range msgs {
		if m.Status == "running" && m.Message == "compositing" {
			sawStatus = true
		}
	}
	if !sawStatus {
		t.Errorf("no running snapshot carried status %q: %+v", "compositing", msgs)
	}
}

func TestExecutor_LogProgressSeesScrubbedLine(t *testing.T) {
	skipOnWindows(t)
	msg := progressAssign("status: token=shh", &protocol.LogProgress{
		Status: []string{`^status: (?P<status>.+)$`},
	})
	msg.Environments = append([]protocol.AssignEnvironment{{
		Name: "exporter",
		OnEnter: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo 'openjd_redacted_env: SECRET=shh'"},
		},
	}}, msg.Environments...)

	msgs, _ := runWithInterceptor(t, msg)
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Message, "shh") {
			t.Errorf("secret leaked into a status message: %q", m.Message)
		}
		if m.Status == "running" && m.Message == "token=<redacted>" {
			found = true
		}
	}
	if !found {
		t.Errorf("no running snapshot carried the scrubbed status: %+v", msgs)
	}
}

func TestExecutor_LogProgressCompileFailureDoesNotFailTask(t *testing.T) {
	skipOnWindows(t)
	// Frame forms need the task's frames; the assignment carries no Frame
	// parameter, so the worker cannot compile the matcher.
	msg := progressAssign("Fra:1|Fra:2", &protocol.LogProgress{
		FrameParameter: "Frame",
		Progress:       []string{`^Fra:(?P<frame>\d+)$`},
	})

	msgs, logs := runWithInterceptor(t, msg)
	last := msgs[len(msgs)-1]
	if last.Status != "succeeded" {
		t.Fatalf("status = %q (%q), want succeeded", last.Status, last.Message)
	}
	for _, m := range msgs {
		if m.LastProgress != nil && m.Status == "running" {
			t.Errorf("a running snapshot carried progress %d", *m.LastProgress)
		}
	}
	const warning = "executor: SQI_LOG_PROGRESS disabled for this attempt"
	if n := strings.Count(logs, warning); n != 1 {
		t.Errorf("warning %q logged %d times, want once; logs:\n%s", warning, n, logs)
	}
	if !strings.Contains(logs, "level=WARN msg=\""+warning) {
		t.Errorf("the diagnostic is not a warning; logs:\n%s", logs)
	}
}

func TestExecutor_LogProgressOnRunOnly(t *testing.T) {
	skipOnWindows(t)
	msg := progressAssign("idle", &protocol.LogProgress{
		Progress: []string{`pct (?P<percent>\d+)`},
		Status:   []string{`pct`},
	})
	msg.Environments = append([]protocol.AssignEnvironment{{
		Name: "noisy",
		OnEnter: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo 'pct 40'; echo 'pct 41' >&2"},
		},
	}}, msg.Environments...)

	msgs, _ := runWithInterceptor(t, msg)
	last := msgs[len(msgs)-1]
	if last.Status != "succeeded" {
		t.Fatalf("status = %q (%q), want succeeded", last.Status, last.Message)
	}
	for _, m := range msgs {
		if m.LastProgress != nil && m.Status == "running" || strings.Contains(m.Message, "pct") {
			t.Errorf("an onEnter line reached the progress matcher: %+v", m)
		}
	}
}

// One line can both set progress and fail the task (spec §5.3).
func TestExecutor_LogChecksAndLogProgressIndependent(t *testing.T) {
	skipOnWindows(t)
	msg := progressAssign("ERROR at 50%", &protocol.LogProgress{
		Progress: []string{`(?P<percent>\d+)%`},
	})
	msg.LogChecks = &protocol.LogChecks{FailOn: []string{"ERROR"}}

	msgs, _ := runWithInterceptor(t, msg)
	last := msgs[len(msgs)-1]
	if last.Status != "failed" {
		t.Fatalf("status = %q (%q), want failed", last.Status, last.Message)
	}
	if want := `log line matched failOn pattern "ERROR": "ERROR at 50%"`; last.Message != want {
		t.Errorf("message = %q, want %q", last.Message, want)
	}
	if last.LastProgress == nil || *last.LastProgress != 50 {
		t.Errorf("terminal LastProgress = %v, want 50", last.LastProgress)
	}
}
