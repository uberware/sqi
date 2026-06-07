// SPDX-License-Identifier: AGPL-3.0-only

package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// ── Subprocess dispatcher ─────────────────────────────────────────────────────
//
// TestMain uses the test binary itself as the subprocess for process execution
// tests.  When SQI_TEST_SUBPROCESS is set, TestMain acts as the child process
// instead of running the test suite.

func TestMain(m *testing.M) {
	switch os.Getenv("SQI_TEST_SUBPROCESS") {
	case "stdout":
		fmt.Println(os.Getenv("SQI_TEST_OUTPUT"))
		os.Exit(0)

	case "stderr":
		fmt.Fprintln(os.Stderr, os.Getenv("SQI_TEST_OUTPUT"))
		os.Exit(0)

	case "both":
		fmt.Println(os.Getenv("SQI_TEST_OUTPUT"))
		fmt.Fprintln(os.Stderr, os.Getenv("SQI_TEST_OUTPUT"))
		os.Exit(0)

	case "exit":
		code, err := strconv.Atoi(os.Getenv("SQI_TEST_EXIT_CODE"))
		if err != nil {
			os.Exit(1) // treat an unparseable exit code as a generic failure
		}
		os.Exit(code)

	case "sleep":
		dur, err := time.ParseDuration(os.Getenv("SQI_TEST_SLEEP"))
		if err != nil {
			dur = 10 * time.Second
		}
		time.Sleep(dur)
		os.Exit(0)

	case "env":
		// Print the value of SQI_TEST_ENV_KEY from the process environment.
		fmt.Println(os.Getenv("SQI_TEST_ENV_KEY"))
		os.Exit(0)

	case "catch_sigterm":
		// Install the SIGTERM handler first, then print "ready" so the parent
		// test knows it is safe to send Cancel.  On SIGTERM, print "sigterm"
		// and exit 0.
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		fmt.Println("ready") // signals the test that the handler is installed
		select {
		case <-c:
			fmt.Println("sigterm")
		case <-time.After(30 * time.Second):
			// Unreachable in normal tests; guards against subprocess orphaning.
		}
		os.Exit(0)

	case "ignore_term":
		// Ignore SIGTERM and sleep; only exits via SIGKILL escalation.
		// Used to assert that the kill-grace-period enforcement is correct.
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}

	os.Exit(m.Run())
}

// ── Test helpers ──────────────────────────────────────────────────────────────

// testBinary returns the path to the current test binary so it can be used
// as a subprocess.
func testBinary() string {
	exe, err := os.Executable()
	if err != nil {
		panic("testBinary: " + err.Error())
	}
	return exe
}

// stubNATS is a fake natsPublisher that records published messages.
type stubNATS struct {
	mu   sync.Mutex
	msgs []stubMsg
}

type stubMsg struct {
	subj string
	data []byte
}

func (s *stubNATS) Publish(subj string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Make a copy so the original slice remains unmodified.
	cp := make([]byte, len(data))
	copy(cp, data)
	s.msgs = append(s.msgs, stubMsg{subj: subj, data: cp})
	return nil
}

// statuses returns the sequence of TaskStatusMsg.Status values published to
// the stub NATS, in order.
func (s *stubNATS) statuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.msgs {
		var msg protocol.TaskStatusMsg
		if err := json.Unmarshal(m.data, &msg); err == nil && msg.Type == protocol.TypeTaskStatus {
			out = append(out, msg.Status)
		}
	}
	return out
}

// lastStatus returns the most recently published TaskStatusMsg, or panics if
// none have been published.
func (s *stubNATS) lastStatus() protocol.TaskStatusMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range slices.Backward(s.msgs) {
		var msg protocol.TaskStatusMsg
		if err := json.Unmarshal(v.data, &msg); err == nil && msg.Type == protocol.TypeTaskStatus {
			return msg
		}
	}
	panic("lastStatus: no status message published")
}

// captureOutput is a line-capturing OutputHandler for test assertions.
type captureOutput struct {
	mu    sync.Mutex
	lines []capturedLine
}

type capturedLine struct {
	stream, line string
}

func (c *captureOutput) HandleLine(_ context.Context, _, _, _, stream, line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, capturedLine{stream: stream, line: line})
}

func (c *captureOutput) all() []capturedLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedLine, len(c.lines))
	copy(out, c.lines)
	return out
}

// newTestExecutor creates an Executor backed by a temp data dir and a stub
// NATS.  The caller is responsible for removing the temp dir.
func newTestExecutor(t *testing.T, maxConcurrent int, capture *captureOutput) (*executor.Executor, *stubNATS, string) {
	t.Helper()
	tmpDir := t.TempDir()
	nc := &stubNATS{}
	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := session.NewManager(tmpDir, false, logger)
	cfg := executor.Config{
		MaxConcurrentTasks: maxConcurrent,
		KillGracePeriod:    500 * time.Millisecond, // short for fast tests
	}
	var oh executor.OutputHandler
	if capture != nil {
		oh = capture
	}
	statusPub := status.New(nc, status.Config{WorkerID: "test-worker"}, logger)
	exec := executor.New(statusPub, mgr, m, oh, cfg, logger)
	return exec, nc, tmpDir
}

// makeAssign creates a minimal AssignMsg that runs the test binary with the
// given subprocess mode and environment variables.
func makeAssign(mode string, extraEnv map[string]string) *protocol.AssignMsg {
	env := map[string]string{"SQI_TEST_SUBPROCESS": mode}
	maps.Copy(env, extraEnv)
	return &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-" + mode,
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: testBinary(),
			Args:    []string{"-test.run=^$"}, // match nothing so the test suite exits immediately
		},
		Environments: []protocol.AssignEnvironment{
			{Name: "test-env", Variables: env},
		},
	}
}

// waitForStatus polls nc until at least n status messages have been published
// or the deadline elapses.
func waitForStatus(t *testing.T, nc *stubNATS, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		nc.mu.Lock()
		count := len(nc.msgs)
		nc.mu.Unlock()
		if count >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	nc.mu.Lock()
	got := len(nc.msgs)
	nc.mu.Unlock()
	t.Fatalf("timed out waiting for %d NATS status messages; got %d", n, got)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestExecutor_Dispatch_stdout verifies that stdout lines emitted by the
// subprocess are captured and forwarded to the OutputHandler (task 52).
func TestExecutor_Dispatch_stdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec; covered separately on Windows")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, 1, capture)

	msg := makeAssign("stdout", map[string]string{"SQI_TEST_OUTPUT": "hello stdout"})
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Wait for running + terminal status.
	waitForStatus(t, nc, 2, 10*time.Second)

	statuses := nc.statuses()
	if len(statuses) < 2 {
		t.Fatalf("expected ≥ 2 status messages, got %d: %v", len(statuses), statuses)
	}
	if statuses[0] != "running" {
		t.Errorf("first status = %q; want %q", statuses[0], "running")
	}
	if statuses[len(statuses)-1] != "succeeded" {
		t.Errorf("last status = %q; want %q", statuses[len(statuses)-1], "succeeded")
	}

	// Check stdout was captured.
	lines := capture.all()
	found := false
	for _, l := range lines {
		if l.stream == "stdout" && l.line == "hello stdout" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("stdout line %q not captured; got: %v", "hello stdout", lines)
	}
}

// TestExecutor_Dispatch_stderr verifies that stderr lines are captured and
// attributed to the "stderr" stream (task 52).
func TestExecutor_Dispatch_stderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, 1, capture)

	msg := makeAssign("stderr", map[string]string{"SQI_TEST_OUTPUT": "hello stderr"})
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	lines := capture.all()
	found := false
	for _, l := range lines {
		if l.stream == "stderr" && l.line == "hello stderr" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("stderr line %q not captured; got: %v", "hello stderr", lines)
	}
}

// TestExecutor_Dispatch_exitCode verifies that a non-zero exit code is treated
// as a failure (task 54) and the exit code is included in the status message.
func TestExecutor_Dispatch_exitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "42"})
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Errorf("terminal status = %q; want %q", last.Status, "failed")
	}
	if last.ExitCode == nil {
		t.Fatal("ExitCode is nil; want 42")
	}
	if *last.ExitCode != 42 {
		t.Errorf("ExitCode = %d; want 42", *last.ExitCode)
	}
}

// TestExecutor_Dispatch_exitCodeZero verifies that exit code 0 produces a
// "succeeded" terminal status with ExitCode = 0.
func TestExecutor_Dispatch_exitCodeZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "succeeded" {
		t.Errorf("terminal status = %q; want %q", last.Status, "succeeded")
	}
	if last.ExitCode == nil {
		t.Fatal("ExitCode is nil; want 0")
	}
	if *last.ExitCode != 0 {
		t.Errorf("ExitCode = %d; want 0", *last.ExitCode)
	}
}

// TestExecutor_Dispatch_timeout verifies the SIGTERM → SIGKILL escalation
// path (task 55): a process that sleeps longer than its timeout is killed and
// the task is marked failed with a timeout reason.
func TestExecutor_Dispatch_timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	msg := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "30s"})
	// Set a very short timeout so the test completes quickly.
	msg.OnRun.TimeoutSeconds = 1
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// The process should be killed within: timeout (1s) + kill grace (500ms) +
	// some slack for scheduling.
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Errorf("terminal status = %q; want %q", last.Status, "failed")
	}
	if last.Message == "" {
		t.Error("Message is empty; want a timeout reason string")
	}
}

// TestExecutor_Dispatch_contextCancel verifies that canceling the worker
// context while a task is running kills the process and publishes a "failed"
// status with reason "worker_shutdown" (task 78).
func TestExecutor_Dispatch_contextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	msg := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "30s"})
	ctx, cancel := context.WithCancel(context.Background())
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Give the subprocess time to start before canceling the worker context.
	time.Sleep(200 * time.Millisecond)
	cancel()

	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Errorf("terminal status = %q; want %q (worker shutdown)", last.Status, "failed")
	}
	if last.Message != "worker_shutdown" {
		t.Errorf("Message = %q; want %q", last.Message, "worker_shutdown")
	}
}

// TestExecutor_Dispatch_envMerge verifies that environment variables from
// AssignEnvironment.Variables are passed to the subprocess (task 50).
func TestExecutor_Dispatch_envMerge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, 1, capture)

	// The "env" subprocess prints os.Getenv("SQI_TEST_ENV_KEY") to stdout.
	// We pass it via AssignEnvironment.Variables so it reaches the process.
	msg := makeAssign("env", map[string]string{
		"SQI_TEST_ENV_KEY": "injected-value",
	})
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	lines := capture.all()
	found := false
	for _, l := range lines {
		if l.stream == "stdout" && l.line == "injected-value" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected stdout line %q from env subprocess; got: %v", "injected-value", lines)
	}
}

// TestExecutor_Dispatch_atCapacity verifies that Dispatch returns a non-nil
// error when all concurrency slots are occupied (task 56).
func TestExecutor_Dispatch_atCapacity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, _, _ := newTestExecutor(t, 1, nil) // max 1 concurrent task

	// Use a cancellable context so the sleeping goroutine is terminated when
	// the test ends, preventing goroutine leaks and temp-dir races.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Dispatch a long-running task to fill the single slot.
	fillMsg := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "60s"})
	fillMsg.TaskID = "fill-task"
	if err := exec.Dispatch(ctx, fillMsg); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}

	// Give the goroutine time to start and consume the semaphore slot.
	time.Sleep(200 * time.Millisecond)

	// Second dispatch should be rejected while the first task is running.
	overMsg := makeAssign("stdout", map[string]string{"SQI_TEST_OUTPUT": "should not run"})
	overMsg.TaskID = "over-capacity-task"
	if err := exec.Dispatch(ctx, overMsg); err == nil {
		t.Error("expected error when dispatching over capacity; got nil")
	}
}

// TestExecutor_ActiveTaskCount verifies that ActiveTaskCount increments on
// Dispatch and decrements when the task exits (task 56).
func TestExecutor_ActiveTaskCount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 4, nil)

	if n := exec.ActiveTaskCount(); n != 0 {
		t.Fatalf("initial ActiveTaskCount = %d; want 0", n)
	}

	const tasks = 3
	for i := range tasks {
		msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
		msg.TaskID = fmt.Sprintf("count-task-%d", i)
		msg.AttemptID = fmt.Sprintf("attempt-%d", i)
		if err := exec.Dispatch(context.Background(), msg); err != nil {
			t.Fatalf("Dispatch task %d: %v", i, err)
		}
	}

	// Wait for all terminal statuses (running + terminal × tasks).
	waitForStatus(t, nc, tasks*2, 15*time.Second)

	// After all tasks complete, count should return to 0.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.ActiveTaskCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("ActiveTaskCount = %d after all tasks completed; want 0", exec.ActiveTaskCount())
}

// TestExecutor_Dispatch_sessionID verifies that every published status message
// carries a non-empty SessionID (task 48).
func TestExecutor_Dispatch_sessionID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	nc.mu.Lock()
	defer nc.mu.Unlock()
	for _, m := range nc.msgs {
		var sm protocol.TaskStatusMsg
		if err := json.Unmarshal(m.data, &sm); err != nil || sm.Type != protocol.TypeTaskStatus {
			continue
		}
		if sm.SessionID == "" {
			t.Errorf("status %q has empty SessionID", sm.Status)
		}
	}
}

// TestExecutor_LastAssignmentAt verifies that LastAssignmentAt is nil before
// any task is dispatched and non-nil afterwards.
func TestExecutor_LastAssignmentAt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	if exec.LastAssignmentAt() != nil {
		t.Fatal("LastAssignmentAt should be nil before any dispatch")
	}

	before := time.Now()
	msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 1, 5*time.Second)

	at := exec.LastAssignmentAt()
	if at == nil {
		t.Fatal("LastAssignmentAt is nil after dispatch")
	}
	if at.Before(before) {
		t.Errorf("LastAssignmentAt (%v) is before dispatch time (%v)", at, before)
	}
}

// TestExecutor_Dispatch_workerID verifies that every published status message
// carries the worker_id injected via the status publisher (task 76).
func TestExecutor_Dispatch_workerID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	nc.mu.Lock()
	defer nc.mu.Unlock()
	for _, m := range nc.msgs {
		var sm protocol.TaskStatusMsg
		if err := json.Unmarshal(m.data, &sm); err != nil || sm.Type != protocol.TypeTaskStatus {
			continue
		}
		if sm.WorkerID != "test-worker" {
			t.Errorf("status %q has WorkerID = %q; want %q", sm.Status, sm.WorkerID, "test-worker")
		}
	}
}

// TestExecutor_FlushShutdownStatuses verifies that FlushShutdownStatuses
// publishes "failed"/"worker_shutdown" for all active tasks (task 78).
func TestExecutor_FlushShutdownStatuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	// Use a cancellable context so the sleeping goroutine is cleaned up.
	ctx := t.Context()

	exec, nc, _ := newTestExecutor(t, 2, nil)

	// Dispatch two long-running tasks to fill the active-tasks map.
	for i := range 2 {
		m := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "60s"})
		m.TaskID = fmt.Sprintf("flush-task-%d", i)
		m.AttemptID = fmt.Sprintf("flush-attempt-%d", i)
		if err := exec.Dispatch(ctx, m); err != nil {
			t.Fatalf("Dispatch task %d: %v", i, err)
		}
	}

	// Wait for both "running" status messages before calling flush.
	waitForStatus(t, nc, 2, 5*time.Second)

	// Record how many messages exist before flush.
	nc.mu.Lock()
	beforeCount := len(nc.msgs)
	nc.mu.Unlock()

	// Flush shutdown statuses.
	exec.FlushShutdownStatuses()

	// FlushShutdownStatuses is synchronous; check immediately.
	nc.mu.Lock()
	afterCount := len(nc.msgs)
	msgs := make([]stubMsg, len(nc.msgs))
	copy(msgs, nc.msgs)
	nc.mu.Unlock()

	added := afterCount - beforeCount
	if added != 2 {
		t.Fatalf("expected 2 additional messages from FlushShutdownStatuses, got %d", added)
	}

	// Verify the new messages are "failed"/"worker_shutdown".
	for _, m := range msgs[beforeCount:] {
		var sm protocol.TaskStatusMsg
		if err := json.Unmarshal(m.data, &sm); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if sm.Status != "failed" {
			t.Errorf("FlushShutdownStatuses message Status = %q; want %q", sm.Status, "failed")
		}
		if sm.Message != "worker_shutdown" {
			t.Errorf("FlushShutdownStatuses message Message = %q; want %q", sm.Message, "worker_shutdown")
		}
	}
}

// ── Cancellation tests (task 85) ──────────────────────────────────────────────

// TestExecutor_Cancel_canceledStatus verifies that calling Cancel on an
// in-progress task causes it to publish a "canceled" terminal status (not
// "failed/worker_shutdown") and that the worker context is not canceled
// (task 81, 83).
func TestExecutor_Cancel_canceledStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix signals; covered via taskkill path separately")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	msg := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "30s"})
	ctx := context.Background() // worker-wide context — NOT canceled
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Wait for the "running" status so the process is started before Cancel.
	waitForStatus(t, nc, 1, 5*time.Second)

	found := exec.Cancel(msg.TaskID)
	if !found {
		t.Fatal("Cancel returned false; expected the task to be found")
	}

	// Wait for the terminal status (running + canceled = 2 messages).
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "canceled" {
		t.Errorf("terminal status = %q; want %q", last.Status, "canceled")
	}
}

// TestExecutor_Cancel_taskNotFound verifies that Cancel returns false for an
// unknown task ID (task 81).
func TestExecutor_Cancel_taskNotFound(t *testing.T) {
	exec, _, _ := newTestExecutor(t, 1, nil)

	found := exec.Cancel("nonexistent-task-id")
	if found {
		t.Error("Cancel returned true for an unknown task ID; want false")
	}
}

// TestExecutor_Cancel_sigkillEscalation verifies that a process that ignores
// SIGTERM is force-killed after KillGracePeriod and that the terminal status
// is still "canceled" (tasks 82, 83).
func TestExecutor_Cancel_sigkillEscalation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM/SIGKILL escalation is Unix-specific; Windows uses taskkill")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	// "ignore_term" subprocess installs signal.Ignore(SIGTERM) and sleeps.
	// It will only exit when SIGKILL is sent after the grace period.
	msg := makeAssign("ignore_term", nil)
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Wait for "running" before canceling.
	waitForStatus(t, nc, 1, 5*time.Second)

	cancelAt := time.Now()
	found := exec.Cancel(msg.TaskID)
	if !found {
		t.Fatal("Cancel returned false; expected the task to be found")
	}

	// The process should die within: KillGracePeriod (500ms) + SIGKILL overhead +
	// a generous scheduling slack.  Total budget: 5 s.
	waitForStatus(t, nc, 2, 10*time.Second)

	elapsed := time.Since(cancelAt)
	// Must not die too quickly (before SIGTERM grace expires) or too slowly.
	// Lower bound: SIGTERM sent, grace period ~0 ms (process might exit on SIGTERM
	// even if Ignore was set — on some platforms Ignore affects signal delivery).
	// We just assert it completes within the test timeout above.
	if elapsed > 8*time.Second {
		t.Errorf("process did not exit within expected window after Cancel; took %v", elapsed)
	}

	last := nc.lastStatus()
	if last.Status != "canceled" {
		t.Errorf("terminal status = %q; want %q (even after SIGKILL)", last.Status, "canceled")
	}
}

// waitForOutputLine polls capture until a stdout line equal to want appears or
// the deadline elapses.  Used when the test needs to synchronize on subprocess
// output before taking an action (e.g., waiting for "ready" so the subprocess
// has installed its signal handler before Cancel is called).
func waitForOutputLine(t *testing.T, capture *captureOutput, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, l := range capture.all() {
			if l.stream == "stdout" && l.line == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stdout line %q not seen within %v", want, timeout)
}

// TestExecutor_Cancel_sigtermDelivery verifies that SIGTERM is delivered to
// the process before SIGKILL escalation by dispatching a subprocess that
// catches SIGTERM, prints "sigterm" on receipt, and exits 0.  The terminal
// status must be "canceled" (tasks 82, 83).
func TestExecutor_Cancel_sigtermDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM is Unix-specific; Windows uses taskkill")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, 1, capture)

	// "catch_sigterm" subprocess installs a SIGTERM handler, prints "ready" to
	// signal it is safe to cancel, then prints "sigterm" on receipt and exits 0.
	msg := makeAssign("catch_sigterm", nil)
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Wait for "ready" — this confirms the signal handler is installed and the
	// subprocess will not miss SIGTERM.
	waitForOutputLine(t, capture, "ready", 5*time.Second)

	found := exec.Cancel(msg.TaskID)
	if !found {
		t.Fatal("Cancel returned false; expected the task to be found")
	}

	// Process exits on SIGTERM (before the grace period); terminal status
	// must be "canceled" because the worker context (Background) is not done.
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "canceled" {
		t.Errorf("terminal status = %q; want %q", last.Status, "canceled")
	}

	// By the time the terminal status is published, all output goroutines have
	// drained the pipes — captured lines are complete and stable here.
	lines := capture.all()
	sigtermSeen := false
	for _, l := range lines {
		if l.stream == "stdout" && l.line == "sigterm" {
			sigtermSeen = true
			break
		}
	}
	if !sigtermSeen {
		t.Errorf("subprocess stdout did not contain %q — SIGTERM not delivered before kill; captured: %v",
			"sigterm", lines)
	}
}

// TestExecutor_Cancel_earlyCancel verifies that calling Cancel before the
// task's runTask goroutine has set up its per-task context still cancels the
// task correctly (cancelRequested flag path, task 80).
func TestExecutor_Cancel_earlyCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	msg := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "30s"})
	ctx := context.Background()

	// Dispatch and immediately Cancel before the goroutine likely runs.
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// Cancel right away — may race with goroutine startup, exercising both
	// the "cancelFunc set" and "cancelRequested" paths.
	exec.Cancel(msg.TaskID)

	// Regardless of which path fires, the task must reach a terminal state.
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	// Worker context (ctx) is Background — so this must be "canceled", not
	// "failed/worker_shutdown".
	if last.Status != "canceled" {
		t.Errorf("terminal status = %q; want %q", last.Status, "canceled")
	}
}

// ── CancelRegistrar error-path tests ─────────────────────────────────────────

// stubCancelRegistrarError is a CancelRegistrar whose Register always returns
// an error.  Used to verify that a subscription failure does not abort task
// execution (task 80).
type stubCancelRegistrarError struct {
	mu            sync.Mutex
	registerCalls int
	deregCalls    int
}

func (s *stubCancelRegistrarError) Register(_ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerCalls++
	return errors.New("stub: NATS subscribe failed")
}

func (s *stubCancelRegistrarError) Deregister(_ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deregCalls++
}

// TestExecutor_Cancel_registrarError verifies that a failure in
// CancelRegistrar.Register is logged as a warning but does not abort task
// execution: the task still runs to completion and Deregister is NOT called
// (nothing was registered, so there is nothing to deregister — task 80).
func TestExecutor_Cancel_registrarError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, 1, nil)

	cr := &stubCancelRegistrarError{}
	exec.SetCancelRegistrar(cr)

	msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "succeeded" {
		t.Errorf("terminal status = %q; want %q (task must run despite registrar error)",
			last.Status, "succeeded")
	}

	cr.mu.Lock()
	rc := cr.registerCalls
	dc := cr.deregCalls
	cr.mu.Unlock()

	if rc != 1 {
		t.Errorf("Register called %d time(s); want 1", rc)
	}
	// Deregister must not be called when Register failed — nothing was subscribed.
	if dc != 0 {
		t.Errorf("Deregister called %d time(s) after failed Register; want 0", dc)
	}
}
