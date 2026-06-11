// SPDX-License-Identifier: AGPL-3.0-or-later

package status_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
)

// ── Test doubles ──────────────────────────────────────────────────────────────

// stubNC is a minimal natsPublisher stub that records published messages and
// can be configured to fail for the first N publish calls.
type stubNC struct {
	mu       sync.Mutex
	msgs     []capturedMsg
	failNext int // number of upcoming Publish calls to return an error
}

type capturedMsg struct {
	subj string
	data []byte
}

func (s *stubNC) Publish(subj string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext > 0 {
		s.failNext--
		return errors.New("transient publish error")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	s.msgs = append(s.msgs, capturedMsg{subj: subj, data: cp})
	return nil
}

func (s *stubNC) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func (s *stubNC) all() []protocol.TaskStatusMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.TaskStatusMsg
	for _, m := range s.msgs {
		var sm protocol.TaskStatusMsg
		if err := json.Unmarshal(m.data, &sm); err == nil {
			out = append(out, sm)
		}
	}
	return out
}

func (s *stubNC) last() protocol.TaskStatusMsg {
	msgs := s.all()
	if len(msgs) == 0 {
		panic("stubNC: no messages published")
	}
	return msgs[len(msgs)-1]
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func makeAssign() *protocol.AssignMsg {
	return &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-1",
		AttemptID: "attempt-1",
		JobID:     "job-1",
	}
}

func newPub(nc *stubNC, workerID string, retryDelay time.Duration) *status.Publisher {
	return status.New(nc, status.Config{
		WorkerID:   workerID,
		MaxRetries: 3,
		RetryDelay: retryDelay,
	}, slog.New(slog.DiscardHandler))
}

// ── Running tests ─────────────────────────────────────────────────────────────

// TestRunning_Fields verifies that Running publishes a "running" status with
// all required fields populated (tasks 75, 76, 77).
func TestRunning_Fields(t *testing.T) {
	nc := &stubNC{}
	pub := newPub(nc, "worker-abc", time.Millisecond)
	at := time.Now().UTC().Truncate(time.Second)

	pub.Running(context.Background(), makeAssign(), "sess-1", nil, at)

	if nc.count() != 1 {
		t.Fatalf("expected 1 published message, got %d", nc.count())
	}
	m := nc.last()

	if m.Status != "running" {
		t.Errorf("Status = %q, want %q", m.Status, "running")
	}
	if m.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want %q", m.TaskID, "task-1")
	}
	if m.AttemptID != "attempt-1" {
		t.Errorf("AttemptID = %q, want %q", m.AttemptID, "attempt-1")
	}
	if m.JobID != "job-1" {
		t.Errorf("JobID = %q, want %q", m.JobID, "job-1")
	}
	if m.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", m.SessionID, "sess-1")
	}
	if m.WorkerID != "worker-abc" {
		t.Errorf("WorkerID = %q, want %q", m.WorkerID, "worker-abc")
	}
	if m.LastProgress != nil {
		t.Errorf("LastProgress = %v, want nil for initial running status", m.LastProgress)
	}
	if m.ExitCode != nil {
		t.Errorf("ExitCode should be nil for running status")
	}
	if m.Type != protocol.TypeTaskStatus {
		t.Errorf("Type = %q, want %q", m.Type, protocol.TypeTaskStatus)
	}
	if m.Version != protocol.ProtocolVersion {
		t.Errorf("Version = %q, want %q", m.Version, protocol.ProtocolVersion)
	}
	if !m.At.Equal(at) {
		t.Errorf("At = %v, want %v", m.At, at)
	}
}

// TestRunning_WithProgress verifies that Running propagates a non-nil lastProgress
// into the message (task 76).
func TestRunning_WithProgress(t *testing.T) {
	nc := &stubNC{}
	pub := newPub(nc, "w", time.Millisecond)
	progress := 42

	pub.Running(context.Background(), makeAssign(), "sess-1", &progress, time.Now())

	m := nc.last()
	if m.LastProgress == nil {
		t.Fatal("LastProgress is nil, want 42")
	}
	if *m.LastProgress != 42 {
		t.Errorf("LastProgress = %d, want 42", *m.LastProgress)
	}
}

// ── Terminal tests ────────────────────────────────────────────────────────────

// TestTerminal_Succeeded verifies the "succeeded" terminal status with required
// fields (task 75, 76).
func TestTerminal_Succeeded(t *testing.T) {
	nc := &stubNC{}
	pub := newPub(nc, "worker-abc", time.Millisecond)
	exitCode := 0

	pub.Terminal(context.Background(), makeAssign(), "sess-1", "succeeded", &exitCode, "", nil, time.Now())

	m := nc.last()
	if m.Status != "succeeded" {
		t.Errorf("Status = %q, want %q", m.Status, "succeeded")
	}
	if m.ExitCode == nil || *m.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", m.ExitCode)
	}
	if m.Message != "" {
		t.Errorf("Message = %q, want empty for succeeded", m.Message)
	}
	if m.WorkerID != "worker-abc" {
		t.Errorf("WorkerID = %q, want %q", m.WorkerID, "worker-abc")
	}
}

// TestTerminal_Failed verifies the "failed" status with exit code, failure
// reason, and last_progress (task 75, 76).
func TestTerminal_Failed(t *testing.T) {
	nc := &stubNC{}
	pub := newPub(nc, "w", time.Millisecond)
	exitCode := 2
	progress := 75

	pub.Terminal(context.Background(), makeAssign(), "sess-1", "failed", &exitCode, "render crashed", &progress, time.Now())

	m := nc.last()
	if m.Status != "failed" {
		t.Errorf("Status = %q, want %q", m.Status, "failed")
	}
	if m.ExitCode == nil || *m.ExitCode != 2 {
		t.Errorf("ExitCode = %v, want 2", m.ExitCode)
	}
	if m.Message != "render crashed" {
		t.Errorf("Message = %q, want %q", m.Message, "render crashed")
	}
	if m.LastProgress == nil || *m.LastProgress != 75 {
		t.Errorf("LastProgress = %v, want 75", m.LastProgress)
	}
}

// TestTerminal_Canceled verifies that "canceled" status has no exit code (task 75).
func TestTerminal_Canceled(t *testing.T) {
	nc := &stubNC{}
	pub := newPub(nc, "w", time.Millisecond)

	pub.Terminal(context.Background(), makeAssign(), "sess-1", "canceled", nil, "", nil, time.Now())

	m := nc.last()
	if m.Status != "canceled" {
		t.Errorf("Status = %q, want %q", m.Status, "canceled")
	}
	if m.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil for canceled", m.ExitCode)
	}
}

// ── ShutdownFailed tests ──────────────────────────────────────────────────────

// TestShutdownFailed_PublishesForEachTask verifies that ShutdownFailed publishes
// one "failed"/"worker_shutdown" message per task (task 78).
func TestShutdownFailed_PublishesForEachTask(t *testing.T) {
	nc := &stubNC{}
	pub := newPub(nc, "worker-abc", time.Millisecond)

	tasks := []status.ShutdownTask{
		{TaskID: "task-1", AttemptID: "attempt-1", JobID: "job-1", SessionID: "sess-1"},
		{TaskID: "task-2", AttemptID: "attempt-2", JobID: "job-1", SessionID: "sess-2"},
		{TaskID: "task-3", AttemptID: "attempt-3", JobID: "job-2", SessionID: "sess-3"},
	}
	pub.ShutdownFailed(context.Background(), tasks)

	msgs := nc.all()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	for idx, m := range msgs {
		if m.Status != "failed" {
			t.Errorf("[%d] Status = %q, want %q", idx, m.Status, "failed")
		}
		if m.Message != "worker_shutdown" {
			t.Errorf("[%d] Message = %q, want %q", idx, m.Message, "worker_shutdown")
		}
		if m.WorkerID != "worker-abc" {
			t.Errorf("[%d] WorkerID = %q, want %q", idx, m.WorkerID, "worker-abc")
		}
		if m.TaskID != tasks[idx].TaskID {
			t.Errorf("[%d] TaskID = %q, want %q", idx, m.TaskID, tasks[idx].TaskID)
		}
		if m.AttemptID != tasks[idx].AttemptID {
			t.Errorf("[%d] AttemptID = %q, want %q", idx, m.AttemptID, tasks[idx].AttemptID)
		}
		if m.SessionID != tasks[idx].SessionID {
			t.Errorf("[%d] SessionID = %q, want %q", idx, m.SessionID, tasks[idx].SessionID)
		}
	}
}

// TestShutdownFailed_NoOp verifies that ShutdownFailed is a no-op for a nil
// or empty task list.
func TestShutdownFailed_NoOp(t *testing.T) {
	nc := &stubNC{}
	pub := newPub(nc, "w", time.Millisecond)

	pub.ShutdownFailed(context.Background(), nil)
	pub.ShutdownFailed(context.Background(), []status.ShutdownTask{})

	if nc.count() != 0 {
		t.Errorf("expected 0 messages for empty task list, got %d", nc.count())
	}
}

// ── Retry tests ───────────────────────────────────────────────────────────────

// TestRetry_SucceedsAfterTransientFailures verifies that publishWithRetry retries
// on transient publish failures and succeeds once the stub recovers (task 79).
func TestRetry_SucceedsAfterTransientFailures(t *testing.T) {
	nc := &stubNC{failNext: 2} // first 2 calls fail; 3rd succeeds
	pub := newPub(nc, "w", time.Millisecond)

	pub.Running(context.Background(), makeAssign(), "sess-1", nil, time.Now())

	// One successful message should have been recorded.
	if nc.count() != 1 {
		t.Errorf("expected 1 published message after retries, got %d", nc.count())
	}
}

// TestRetry_AllFail verifies that after MaxRetries all fail, no message is
// published and the function returns without blocking (task 79).
func TestRetry_AllFail(t *testing.T) {
	nc := &stubNC{failNext: 999} // always fails
	pub := status.New(nc, status.Config{
		WorkerID:   "w",
		MaxRetries: 3,
		RetryDelay: time.Millisecond, // fast for tests
	}, slog.New(slog.DiscardHandler))

	pub.Running(context.Background(), makeAssign(), "sess-1", nil, time.Now())

	if nc.count() != 0 {
		t.Errorf("expected 0 published messages (all retries failed), got %d", nc.count())
	}
}

// TestRetry_ContextCanceled verifies that a canceled context short-circuits the
// retry backoff (task 79).
func TestRetry_ContextCanceled(t *testing.T) {
	nc := &stubNC{failNext: 999}
	pub := status.New(nc, status.Config{
		WorkerID:   "w",
		MaxRetries: 3,
		RetryDelay: 500 * time.Millisecond, // long delay that should be short-circuited
	}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before publish so ctx is already done

	start := time.Now()
	pub.Running(ctx, makeAssign(), "sess-1", nil, time.Now())
	elapsed := time.Since(start)

	// Should return almost immediately — not wait for 500ms × 3 retries.
	if elapsed > 200*time.Millisecond {
		t.Errorf("expected fast abort on canceled context, took %v", elapsed)
	}
}

// TestRetry_ExactlyMaxRetries verifies the retry count ceiling: with failNext=3
// and MaxRetries=3, the 4th attempt (the 3rd retry) succeeds.
func TestRetry_ExactlyMaxRetries(t *testing.T) {
	nc := &stubNC{failNext: 3} // first 3 calls fail; 4th succeeds
	pub := status.New(nc, status.Config{
		WorkerID:   "w",
		MaxRetries: 3,
		RetryDelay: time.Millisecond,
	}, slog.New(slog.DiscardHandler))

	pub.Running(context.Background(), makeAssign(), "sess-1", nil, time.Now())

	if nc.count() != 1 {
		t.Errorf("expected 1 published message (succeeded on 4th attempt), got %d", nc.count())
	}
}
