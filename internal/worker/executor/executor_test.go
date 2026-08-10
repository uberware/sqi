// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/status"
)

// ── S3 path guard tests ───────────────────────────────────────────────────────

// TestExecutor_Dispatch_S3PathWithoutStaging verifies that a resolved s3://
// path with no stage_locally delivery fails the task pre-exec with a clear
// reason, mirroring the other pre-exec failures.
func TestExecutor_Dispatch_S3PathWithoutStaging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}
	exec, nc, _ := newTestExecutor(t, nil)

	msg := makeAssign("stdout", nil)
	msg.OnRun.Args = []string{"s3://studio-bucket/shows/scene.hip"}
	// No PathDeliveries → default (no stage_locally).

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Fatalf("terminal status = %q, want failed", last.Status)
	}
	if !strings.Contains(last.Message, "stage_locally") ||
		!strings.Contains(last.Message, "s3://studio-bucket/shows/scene.hip") {
		t.Errorf("message = %q; want it to name the path and stage_locally", last.Message)
	}
	if statuses := nc.statuses(); len(statuses) < 2 || statuses[0] != "running" {
		t.Errorf("want running→failed transition, got %v", statuses)
	}
}

// TestExecutor_firstS3Path verifies that FirstS3Path finds the first s3:// URI
// in a resolved action's command, args, and env-var values.
// With stage_locally enabled the guard does not trip on the pre-swap s3:// path
// (staging is responsible for rewriting it to a scratch path).
func TestExecutor_firstS3Path(t *testing.T) {
	action := &protocol.Action{Command: "/bin/echo", Args: []string{"--in", "s3://b/x"}}
	if p, ok := executor.FirstS3Path(action, nil); !ok || p != "s3://b/x" {
		t.Errorf("FirstS3Path args = (%q,%v), want (s3://b/x,true)", p, ok)
	}
	if p, ok := executor.FirstS3Path(&protocol.Action{Command: "/bin/echo"}, map[string]string{"IN": "s3://b/y"}); !ok || p != "s3://b/y" {
		t.Errorf("FirstS3Path env = (%q,%v), want (s3://b/y,true)", p, ok)
	}
	if _, ok := executor.FirstS3Path(&protocol.Action{Command: "/bin/echo", Args: []string{"/mnt/nas/x"}}, nil); ok {
		t.Error("FirstS3Path should not match a filesystem path")
	}
}

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
		// Install SIGTERM handler first, then print "ready" so the parent
		// test knows the handler is installed before it calls Cancel.
		// signal.Notify is used (not signal.Ignore) so the Go runtime
		// captures SIGTERM into our channel rather than relying on SIG_IGN,
		// which the runtime may reset in the test-binary harness.
		// The drain loop keeps the process alive until SIGKILL arrives.
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		fmt.Println("ready") // signals the test that the handler is installed
		timeout := time.After(60 * time.Second)
		for {
			select {
			case <-c: // SIGTERM received — drain but do not exit
			case <-timeout:
				os.Exit(0) // unreachable in normal tests; guards against orphaning
			}
		}
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
func newTestExecutor(t *testing.T, capture *captureOutput) (*executor.Executor, *stubNATS, string) {
	t.Helper()
	return newTestExecutorLimited(t, capture, fmtres.ExprLimits{})
}

// newTestExecutorWithExprLimits is newTestExecutor with this host's phase-3
// expression limits configured -- EXPR sub-project E4d's Task 2. Callers that
// do not care about them keep using newTestExecutor, which passes the zero
// value (the built-in defaults).
func newTestExecutorWithExprLimits(t *testing.T, lim fmtres.ExprLimits) (*executor.Executor, *stubNATS, string) {
	t.Helper()
	return newTestExecutorLimited(t, nil, lim)
}

func newTestExecutorLimited(
	t *testing.T, capture *captureOutput, lim fmtres.ExprLimits,
) (*executor.Executor, *stubNATS, string) {
	t.Helper()
	tmpDir := t.TempDir()
	nc := &stubNATS{}
	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := session.NewManager(filepath.Join(tmpDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, lim, logger)
	cfg := executor.Config{
		KillGracePeriod: 500 * time.Millisecond, // short for fast tests
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
// subprocess are captured and forwarded to the OutputHandler.
func TestExecutor_Dispatch_stdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec; covered separately on Windows")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

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
// attributed to the "stderr" stream.
func TestExecutor_Dispatch_stderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

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
// as a failure and the exit code is included in the status message.
func TestExecutor_Dispatch_exitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

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

	exec, nc, _ := newTestExecutor(t, nil)

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

// TestExecutor_Dispatch_timeout verifies the per-task timeout termination path:
// a process with no cancelation method (the OpenJD TERMINATE default → immediate
// SIGKILL) that sleeps longer than its timeout is killed and the task is marked
// failed with a timeout reason.
func TestExecutor_Dispatch_timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

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

// TestExecutor_DrainAndShutdown_workerShutdown verifies that DrainAndShutdown
// with a zero grace period force-kills in-flight tasks and causes them to
// publish a "failed"/"worker_shutdown" terminal status.
//
// An earlier version of this test canceled the worker context directly; the
// current design decouples task execution from the signal context so that
// tasks survive SIGINT/SIGTERM and are only killed by DrainAndShutdown.
func TestExecutor_DrainAndShutdown_workerShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

	msg := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "30s"})
	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Wait for the subprocess to start (running status).
	waitForStatus(t, nc, 1, 5*time.Second)

	// DrainAndShutdown(0): zero grace period → force-kill immediately.
	completed, killed := exec.DrainAndShutdown(0)
	if killed != 1 {
		t.Errorf("DrainAndShutdown killed = %d; want 1", killed)
	}
	if completed != 0 {
		t.Errorf("DrainAndShutdown completed = %d; want 0", completed)
	}

	// DrainAndShutdown blocks until all goroutines exit; terminal status is
	// already published.
	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Errorf("terminal status = %q; want %q (worker shutdown)", last.Status, "failed")
	}
	if last.Message != "worker_shutdown" {
		t.Errorf("Message = %q; want %q", last.Message, "worker_shutdown")
	}
}

// TestExecutor_DrainAndShutdown_allComplete verifies that when all tasks
// complete within the grace period, DrainAndShutdown returns the correct
// completed count with zero killed.
func TestExecutor_DrainAndShutdown_allComplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

	// Both tasks exit immediately.
	for i := range 2 {
		m := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
		m.TaskID = fmt.Sprintf("drain-task-%d", i)
		m.AttemptID = fmt.Sprintf("drain-attempt-%d", i)
		if err := exec.Dispatch(context.Background(), m); err != nil {
			t.Fatalf("Dispatch task %d: %v", i, err)
		}
	}

	// Grace period long enough for both fast tasks to complete.
	completed, killed := exec.DrainAndShutdown(10 * time.Second)

	if killed != 0 {
		t.Errorf("killed = %d; want 0 (all tasks should complete within grace period)", killed)
	}
	if completed != 2 {
		t.Errorf("completed = %d; want 2", completed)
	}

	// Both tasks should have published succeeded status.
	_ = nc
}

// TestExecutor_DrainAndShutdown_mixed verifies that tasks completing within the
// grace period are counted as completed and tasks still running after the grace
// period are counted as killed and publish "failed"/"worker_shutdown".
//
// Both tasks are dispatched and confirmed started before DrainAndShutdown is
// called so that both are in the initial snapshot.  The "medium" task sleeps
// 300 ms — long enough to still be running when the drain starts, but short
// enough to complete within the 2 s grace period.  The slow task sleeps 60 s
// and is force-killed after the grace period.
func TestExecutor_DrainAndShutdown_mixed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	// Two concurrent slots.
	exec, nc, _ := newTestExecutor(t, nil)

	// Medium task: sleeps 300 ms → completes naturally during the 2 s grace period.
	medMsg := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "300ms"})
	medMsg.TaskID = "mixed-med-task"
	medMsg.AttemptID = "mixed-med-attempt"
	if err := exec.Dispatch(context.Background(), medMsg); err != nil {
		t.Fatalf("Dispatch medium task: %v", err)
	}

	// Slow task: sleeps 60 s → force-killed after grace period.
	slowMsg := makeAssign("sleep", map[string]string{"SQI_TEST_SLEEP": "60s"})
	slowMsg.TaskID = "mixed-slow-task"
	slowMsg.AttemptID = "mixed-slow-attempt"
	if err := exec.Dispatch(context.Background(), slowMsg); err != nil {
		t.Fatalf("Dispatch slow task: %v", err)
	}

	// Wait for both "running" statuses so both processes are started and both
	// tasks are in activeTasks before DrainAndShutdown captures the initial count.
	waitForStatus(t, nc, 2, 5*time.Second)
	if n := exec.ActiveTaskCount(); n != 2 {
		t.Fatalf("ActiveTaskCount = %d after running; want 2", n)
	}

	// Grace period: 2 s (long enough for the 300 ms task, not for the 60 s task).
	completed, killed := exec.DrainAndShutdown(2 * time.Second)

	if killed != 1 {
		t.Errorf("killed = %d; want 1 (the slow task)", killed)
	}
	if completed != 1 {
		t.Errorf("completed = %d; want 1 (the medium task)", completed)
	}

	// The slow task must have published "failed"/"worker_shutdown".
	nc.mu.Lock()
	defer nc.mu.Unlock()
	var slowTerminal *protocol.TaskStatusMsg
	for _, m := range nc.msgs {
		var sm protocol.TaskStatusMsg
		if err := json.Unmarshal(m.data, &sm); err != nil || sm.Type != protocol.TypeTaskStatus {
			continue
		}
		if sm.TaskID == slowMsg.TaskID && sm.Status != "running" {
			cp := sm
			slowTerminal = &cp
		}
	}
	if slowTerminal == nil {
		t.Fatal("no terminal status found for the slow (force-killed) task")
	}
	if slowTerminal.Status != "failed" {
		t.Errorf("slow task terminal status = %q; want %q", slowTerminal.Status, "failed")
	}
	if slowTerminal.Message != "worker_shutdown" {
		t.Errorf("slow task Message = %q; want %q", slowTerminal.Message, "worker_shutdown")
	}
}

// TestExecutor_Dispatch_envMerge verifies that environment variables from
// AssignEnvironment.Variables are passed to the subprocess.
func TestExecutor_Dispatch_envMerge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

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

// TestExecutor_Dispatch_openjdEnvDirective verifies that an environment variable
// exported by an environment onEnter via an openjd_env directive is visible in
// the task OnRun process environment.
func TestExecutor_Dispatch_openjdEnvDirective(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	// The "setup" environment's onEnter emits an openjd_env directive on stdout.
	// The "vars" environment statically sets SQI_TEST_SUBPROCESS so the OnRun
	// subprocess runs in "env" mode and prints SQI_TEST_ENV_KEY — which is set
	// only by the directive, proving directive-exported vars reach the task.
	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-envdir",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: testBinary(),
			Args:    []string{"-test.run=^$"},
		},
		Environments: []protocol.AssignEnvironment{
			{
				Name: "setup",
				OnEnter: &protocol.Action{
					Command: "sh",
					Args:    []string{"-c", "echo 'openjd_env: SQI_TEST_ENV_KEY=from-directive'"},
				},
			},
			{
				Name:      "vars",
				Variables: map[string]string{"SQI_TEST_SUBPROCESS": "env"},
			},
		},
	}

	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	lines := capture.all()
	found := false
	for _, l := range lines {
		if l.stream == "stdout" && l.line == "from-directive" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected OnRun to see SQI_TEST_ENV_KEY=from-directive from openjd_env directive; got: %v", lines)
	}
}

// TestExecutor_ActiveTaskCount verifies that ActiveTaskCount increments on
// Dispatch and decrements when the task exits.
func TestExecutor_ActiveTaskCount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

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
// carries a non-empty SessionID.
func TestExecutor_Dispatch_sessionID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

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

	exec, nc, _ := newTestExecutor(t, nil)

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
// carries the worker_id injected via the status publisher.
func TestExecutor_Dispatch_workerID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

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
// publishes "failed"/"worker_shutdown" for all active tasks.
func TestExecutor_FlushShutdownStatuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)
	// Ensure sleeping subprocesses are force-killed when the test ends.
	// DrainAndShutdown(0) waits until all goroutines exit before returning,
	// preventing goroutine leaks and temp-dir races.
	t.Cleanup(func() { exec.DrainAndShutdown(0) })

	ctx := context.Background()

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

// ── Cancellation tests ──────────────────────────────────────────────

// TestExecutor_Cancel_canceledStatus verifies that calling Cancel on an
// in-progress task causes it to publish a "canceled" terminal status (not
// "failed/worker_shutdown") and that the worker context is not canceled.
func TestExecutor_Cancel_canceledStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix signals; covered via taskkill path separately")
	}

	exec, nc, _ := newTestExecutor(t, nil)

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
// unknown task ID.
func TestExecutor_Cancel_taskNotFound(t *testing.T) {
	exec, _, _ := newTestExecutor(t, nil)

	found := exec.Cancel("nonexistent-task-id")
	if found {
		t.Error("Cancel returned true for an unknown task ID; want false")
	}
}

// TestExecutor_Cancel_sigkillEscalation verifies that under NOTIFY_THEN_TERMINATE
// a process that ignores SIGTERM is force-killed after the notify period and that
// the terminal status is still "canceled".
func TestExecutor_Cancel_sigkillEscalation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM/SIGKILL escalation is Unix-specific; Windows uses taskkill")
	}

	// captureOutput is required here so we can wait for the "ready" line
	// that the subprocess prints after installing its SIGTERM handler.
	// Waiting for "ready" (not just "running") avoids the race between
	// signal.Notify and SIGTERM delivery that arises because "running" is
	// published before cmd.Start(), leaving the subprocess with almost no
	// time to set up signal handling before Cancel is called — especially
	// under the -race build which slows Go runtime initialization.
	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	// "ignore_term" subprocess installs a SIGTERM handler, prints "ready",
	// and then drains SIGTERM in a loop without exiting.  It exits only
	// when SIGKILL is sent after the notify period.  With
	// NOTIFY_THEN_TERMINATE the executor sends SIGTERM (drained), then
	// escalates to SIGKILL after the notify period.
	msg := makeAssign("ignore_term", nil)
	msg.OnRun.Cancelation = &protocol.CancelationMethod{
		Mode:                "NOTIFY_THEN_TERMINATE",
		NotifyPeriodSeconds: 1, // short notify period for a fast test
	}
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Wait for "ready" — this confirms signal.Notify has been called in the
	// subprocess and it is safe to send Cancel without a race on signal delivery.
	waitForOutputLine(t, capture, "ready", 5*time.Second)

	cancelAt := time.Now()
	found := exec.Cancel(msg.TaskID)
	if !found {
		t.Fatal("Cancel returned false; expected the task to be found")
	}

	// The process should die within: notify period (1s) + SIGKILL overhead +
	// a generous scheduling slack.
	waitForStatus(t, nc, 2, 10*time.Second)

	elapsed := time.Since(cancelAt)
	// Lower bound: the subprocess drains SIGTERM without exiting and must
	// survive until SIGKILL arrives after the 1s notify period.  700ms is
	// generous enough to avoid flakiness under -race while still proving
	// that the grace timer fired before SIGKILL was sent.
	if elapsed < 700*time.Millisecond {
		t.Errorf("process exited in %v — SIGKILL arrived before the 1s notify period elapsed, "+
			"suggesting the SIGTERM→SIGKILL escalation path was not exercised", elapsed)
	}
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

// TestExecutor_Cancel_sigtermDelivery verifies that under NOTIFY_THEN_TERMINATE
// SIGTERM is delivered to the process before SIGKILL escalation, by dispatching a
// subprocess that catches SIGTERM, prints "sigterm" on receipt, and exits 0.  The
// terminal status must be "canceled".
func TestExecutor_Cancel_sigtermDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM is Unix-specific; Windows uses taskkill")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	// "catch_sigterm" subprocess installs a SIGTERM handler, prints "ready" to
	// signal it is safe to cancel, then prints "sigterm" on receipt and exits 0.
	// NOTIFY_THEN_TERMINATE makes the executor send SIGTERM (notify) first.
	msg := makeAssign("catch_sigterm", nil)
	msg.OnRun.Cancelation = &protocol.CancelationMethod{
		Mode:                "NOTIFY_THEN_TERMINATE",
		NotifyPeriodSeconds: 5, // generous so SIGTERM handler runs before SIGKILL
	}
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

// TestExecutor_Cancel_terminateImmediate verifies the OpenJD TERMINATE default
// (no cancelation method specified): the process is SIGKILLed immediately on
// cancel, with NO SIGTERM sent and NO grace period waited.
//
// The subprocess ignores SIGTERM and sleeps 60 s, so under the previous graceful
// default it would survive until the KillGracePeriod (500 ms) elapsed and a
// SIGKILL escalated.  Under the TERMINATE default it is SIGKILLed at once, so the
// process must exit well inside the grace window — proving no SIGTERM/grace wait.
// The terminal status is "canceled".
func TestExecutor_Cancel_terminateImmediate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM/SIGKILL semantics are Unix-specific; Windows uses taskkill")
	}

	exec, nc, _ := newTestExecutor(t, nil) // KillGracePeriod = 500ms

	// No Cancelation set → TERMINATE default → immediate SIGKILL.
	msg := makeAssign("ignore_term", nil)
	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Wait for "running" before canceling.
	waitForStatus(t, nc, 1, 5*time.Second)

	cancelAt := time.Now()
	if found := exec.Cancel(msg.TaskID); !found {
		t.Fatal("Cancel returned false; expected the task to be found")
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	// Immediate SIGKILL: the SIGTERM-ignoring process must NOT be waited on for
	// the 500 ms KillGracePeriod.  Generous upper bound for scheduling/-race slack
	// while still well below the old grace-then-kill timing.
	// 400ms < 500ms KillGracePeriod: any SIGTERM+grace path would exceed this
	// bound, proving immediate SIGKILL with no SIGTERM or grace wait.
	if elapsed := time.Since(cancelAt); elapsed > 400*time.Millisecond {
		t.Errorf("process took %v to exit after Cancel; want prompt SIGKILL with no grace wait", elapsed)
	}

	last := nc.lastStatus()
	if last.Status != "canceled" {
		t.Errorf("terminal status = %q; want %q", last.Status, "canceled")
	}
}

// TestExecutor_Cancel_earlyCancel verifies that calling Cancel before the
// task's runTask goroutine has set up its per-task context still cancels the
// task correctly (cancelRequested flag path).
func TestExecutor_Cancel_earlyCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

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

// ── Embedded files ────────────────────────────────────────────────────────────

// TestExecutor_Dispatch_embeddedFiles_content verifies that EmbeddedFiles in
// AssignMsg are materialized to the session working directory before OnRun
// executes, allowing the task command to read them.
func TestExecutor_Dispatch_embeddedFiles_content(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-embeds-content",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "cat data.txt"},
		},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "data.txt", Data: "sentinel-value\n"},
		},
	}

	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "succeeded" {
		t.Errorf("terminal status = %q; want %q", last.Status, "succeeded")
	}

	lines := capture.all()
	found := false
	for _, l := range lines {
		if l.stream == "stdout" && l.line == "sentinel-value" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected embedded file content %q in stdout; got: %v", "sentinel-value", lines)
	}
}

// TestExecutor_Dispatch_embeddedFiles_runnable verifies that an embedded file
// with Runnable=true is materialized with the execute permission bit set,
// allowing OnRun to execute it directly.
func TestExecutor_Dispatch_embeddedFiles_runnable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-embeds-runnable",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "./render.sh"},
		},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "render.sh", Data: "#!/bin/sh\necho script-ran\n", Runnable: true},
		},
	}

	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "succeeded" {
		t.Errorf("terminal status = %q; want %q (runnable embedded script should execute)", last.Status, "succeeded")
	}

	lines := capture.all()
	found := false
	for _, l := range lines {
		if l.stream == "stdout" && l.line == "script-ran" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected stdout %q from runnable embedded script; got: %v", "script-ran", lines)
	}
}

// TestExecutor_Dispatch_embeddedFiles_eol verifies that EndOfLine conversion is
// applied when embedded files are materialized: data with CRLF line endings and
// EndOfLine="LF" is stored with LF endings in the working directory.
func TestExecutor_Dispatch_embeddedFiles_eol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	capture := &captureOutput{}
	exec, nc, _ := newTestExecutor(t, capture)

	// The embedded file carries CRLF data with EndOfLine="LF".
	// OnRun outputs the byte count of the file; after LF conversion "line1\nline2\n"
	// is 12 bytes, whereas the CRLF original "line1\r\nline2\r\n" would be 14.
	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-embeds-eol",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: "sh",
			// wc -c counts bytes; "line1\nline2\n" = 12 bytes.
			Args: []string{"-c", "wc -c < eol.txt | tr -d ' '"},
		},
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "eol.txt", Data: "line1\r\nline2\r\n", EndOfLine: "LF"},
		},
	}

	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "succeeded" {
		t.Errorf("terminal status = %q; want %q", last.Status, "succeeded")
	}

	// After LF conversion: "line1\nline2\n" = 12 bytes.
	lines := capture.all()
	found := false
	for _, l := range lines {
		if l.stream == "stdout" && l.line == "12" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected byte count %q (LF conversion applied); got: %v", "12", lines)
	}
}

// TestExecutor_Dispatch_embeddedFiles_writeFail verifies that a write failure
// during step-level embedded-file materialization fails the task cleanly with a
// running→failed status transition, without panicking.
func TestExecutor_Dispatch_embeddedFiles_writeFail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell commands")
	}

	exec, nc, _ := newTestExecutor(t, nil)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-embeds-writefail",
		AttemptID: "attempt-1",
		JobID:     "job-1",
		OnRun: &protocol.Action{
			Command: "sh",
			Args:    []string{"-c", "echo should-not-run"},
		},
		// Path traversal filename triggers validation failure in writeEmbeddedFile.
		EmbeddedFiles: []protocol.EmbeddedFile{
			{Name: "evil", Filename: "../escape.txt", Data: "bad"},
		},
	}

	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Pre-execution failures still publish running→failed (2 status messages).
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Errorf("terminal status = %q; want %q (path traversal should fail task cleanly)", last.Status, "failed")
	}
	if last.ExitCode == nil {
		t.Fatal("ExitCode is nil; want -1")
	}
	if *last.ExitCode != -1 {
		t.Errorf("ExitCode = %d; want -1", *last.ExitCode)
	}
	const wantSub = "must not contain path separators"
	if !strings.Contains(last.Message, wantSub) {
		t.Errorf("Message = %q; want it to contain %q", last.Message, wantSub)
	}
}

// ── Dispatch-time pre-execution failure tests ────────────────────────────────
//
// Unlike the pre-exec failures above (which happen inside the task goroutine,
// after Dispatch has already returned nil), these cover a failure in
// session.Manager.Create itself — synchronous, inside Dispatch, before any
// taskRun exists. Before the fix, Dispatch just returned the error to its
// caller (the lease loop), which logged a warning and dropped it: no status
// was ever published, so the task sat in "assigned" until the server's
// heartbeat/lease sweep reclaimed and retried it, eventually surfacing only
// as a bare timeout instead of the actual reason.

// newCredentialFailureSessionRoot builds a session root directory that
// isolation.ValidateTraversable (called from session.Manager.Create, before
// any credential is resolved, the moment an assignment carries run-as-user
// isolation) will always accept, on any machine, without chmod'ing anything
// this test does not itself own.
//
// t.TempDir() cannot be used for this: on macOS it sits several
// testing/OS-owned layers under $TMPDIR (e.g. /var/folders/<x>/<y>/T), each
// hardcoded 0700 by construction, and some sandboxed dev environments refuse
// to widen those regardless of Unix ownership — chmod succeeds but the mode
// is silently unchanged. That made the credential-resolution-failure test
// this fixture supports skip via t.Skipf on such machines, proving nothing
// there. Rooting instead directly under the OS-standard, universally
// 1777 /tmp sidesteps the problem rather than working around it: this test
// only ever needs to widen the ONE directory it creates (MkdirTemp always
// creates 0700 regardless of what mode is requested), because every ancestor
// above it (/tmp itself, and above that /, both effectively fixed system
// directories) is already traversable by design on every POSIX system.
func newCredentialFailureSessionRoot(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("/tmp", "sqi-credfail-*") //nolint:usetesting // t.TempDir() is exactly what this must NOT use — see doc above
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o711); err != nil {
		t.Fatalf("chmod %q: %v", parent, err)
	}
	return parent
}

// TestExecutor_Dispatch_credentialResolutionFailure verifies that a
// run-as-user credential the isolation provider cannot resolve — isolation's
// headline pre-execution error path — now surfaces as a terminal task
// failure carrying the original error text.
func TestExecutor_Dispatch_credentialResolutionFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("run-as-user isolation and its POSIX ancestor-traversability rules are Unix-specific")
	}

	parent := newCredentialFailureSessionRoot(t)
	nc := &stubNATS{}
	m := metrics.New()
	logger := slog.New(slog.DiscardHandler)
	// isolation.NewFake(nil): no known accounts, so Resolve always errors —
	// the injected Provider failure this test is built to drive.
	mgr := session.NewManager(filepath.Join(parent, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	statusPub := status.New(nc, status.Config{WorkerID: "test-worker"}, logger)
	exec := executor.New(statusPub, mgr, m, nil, executor.Config{}, logger)

	msg := makeAssign("stdout", nil)
	msg.Isolation = &protocol.IsolationSpec{User: "render-svc"}

	err := exec.Dispatch(context.Background(), msg)
	if err == nil {
		t.Fatal("Dispatch: want error, got nil")
	}
	if !strings.Contains(err.Error(), "render-svc") {
		t.Errorf("Dispatch error = %q; want it to name the user", err.Error())
	}

	statuses := nc.statuses()
	if len(statuses) != 2 || statuses[0] != "running" || statuses[1] != "failed" {
		t.Fatalf("statuses = %v, want [running failed]", statuses)
	}

	last := nc.lastStatus()
	if last.ExitCode == nil || *last.ExitCode != -1 {
		t.Fatalf("ExitCode = %v, want -1", last.ExitCode)
	}
	if last.SessionID != "" {
		t.Errorf("SessionID = %q, want empty (session was never created)", last.SessionID)
	}
	if !strings.Contains(last.Message, "render-svc") || !strings.Contains(last.Message, "resolve run-as-user") {
		t.Errorf("Message = %q; want it to name the credential-resolution failure and the user", last.Message)
	}

	// The two statuses published above must form a legal state-machine
	// transition pair (assigned→running, then running→failed) so the
	// server's task-status consumer accepts them instead of discarding the
	// write as store.ErrInvalidTransition — see internal/store/statemachine.go.
	if err := store.ValidateTaskTransition(store.TaskStatusAssigned, store.TaskStatusRunning); err != nil {
		t.Errorf("assigned→running must be legal: %v", err)
	}
	if err := store.ValidateTaskTransition(store.TaskStatusRunning, store.TaskStatusFailed); err != nil {
		t.Errorf("running→failed must be legal: %v", err)
	}
}

// failingNATS is a natsPublisher stub whose Publish always fails, for
// exercising the branch where publishing the failure itself fails.
type failingNATS struct {
	mu    sync.Mutex
	calls int
}

func (f *failingNATS) Publish(_ string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return errors.New("stub: publish always fails")
}

// TestExecutor_Dispatch_publishFailureDoesNotWedge verifies that when the
// underlying status publish itself fails (e.g. NATS unreachable), Dispatch
// still returns promptly with the original dispatch error instead of
// blocking or panicking: status.Publisher retries transient failures
// internally and then logs and gives up — status loss is preferred over
// wedging the caller (the lease loop).
func TestExecutor_Dispatch_publishFailureDoesNotWedge(t *testing.T) {
	tmpDir := t.TempDir()
	nc := &failingNATS{}
	m := metrics.New()
	logger := slog.New(slog.DiscardHandler)
	mgr := session.NewManager(filepath.Join(tmpDir, "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	statusPub := status.New(nc, status.Config{WorkerID: "test-worker", MaxRetries: 1, RetryDelay: time.Millisecond}, logger)
	exec := executor.New(statusPub, mgr, m, nil, executor.Config{}, logger)

	msg := makeAssign("stdout", nil)
	msg.Isolation = &protocol.IsolationSpec{User: "render-svc"}

	done := make(chan error, 1)
	go func() { done <- exec.Dispatch(context.Background(), msg) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Dispatch: want error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dispatch did not return: a publish failure appears to have wedged it")
	}

	nc.mu.Lock()
	calls := nc.calls
	nc.mu.Unlock()
	if calls == 0 {
		t.Error("want at least one publish attempt")
	}
}

// ── CancelRegistrar error-path tests ─────────────────────────────────────────

// stubCancelRegistrarError is a CancelRegistrar whose Register always returns
// an error.  Used to verify that a subscription failure does not abort task
// execution.
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
// (nothing was registered, so there is nothing to deregister).
func TestExecutor_Cancel_registrarError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	exec, nc, _ := newTestExecutor(t, nil)

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

// ── Staging scratch-dir cleanup regression test ───────────────────────────────

// TestExecutor_Dispatch_stageScratchCleanedOnPipelineFailure is a regression
// test for the scratch-directory leak: when StageIn succeeds but a subsequent
// step in buildEffectiveLookup fails (here: a PathMap rule with a non-empty
// SourcePath but empty DestinationPath causes pathmap.NewLookup to return an
// error), the scratch directory must still be removed by the caller's deferred
// stager.Cleanup.
func TestExecutor_Dispatch_stageScratchCleanedOnPipelineFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX-style path rules")
	}

	scratchBase := t.TempDir()
	nc := &stubNATS{}
	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := session.NewManager(filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	cfg := executor.Config{
		KillGracePeriod: 500 * time.Millisecond,
		// Staging configured so the stager.Configured() check passes and
		// StageIn creates the scratch dir. The sync command is never actually
		// invoked because there are no Staging entries to copy.
		StagingScratchDir:  scratchBase,
		StagingSyncCommand: "echo {src} {dest}",
	}
	statusPub := status.New(nc, status.Config{WorkerID: "test-worker"}, logger)
	exec := executor.New(statusPub, mgr, m, nil, cfg, logger)

	const jobID = "job-scratch-leak"
	const attemptID = "attempt-1"

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		TaskID:    "task-scratch-leak",
		AttemptID: attemptID,
		JobID:     jobID,
		OnRun:     &protocol.Action{Command: "echo", Args: []string{"should-not-run"}},
		PathDeliveries: []protocol.PathDelivery{
			{Kind: "stage_locally"},
		},
		// No Staging entries: StageIn creates the scratch dir but copies nothing.
		// A PathMap rule with a non-empty SourcePath and empty DestinationPath
		// causes pathmap.NewLookup to fail after StageIn has already created
		// the scratch directory — the scenario that previously leaked the dir.
		PathMap: []protocol.PathMapRule{
			{SourcePath: "/original/path", DestinationPath: ""},
		},
	}

	ctx := context.Background()
	if err := exec.Dispatch(ctx, msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Pre-exec failure publishes running → failed (2 status messages).
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Errorf("terminal status = %q; want %q (pipeline failure must fail the task)", last.Status, "failed")
	}

	// The scratch directory must have been removed by the deferred Cleanup.
	scratchDir := filepath.Join(scratchBase, jobID, attemptID)
	if _, err := os.Stat(scratchDir); !os.IsNotExist(err) {
		t.Errorf("scratch dir %q still exists after pipeline failure — stager.Cleanup was not called (leak)", scratchDir)
	}
}

// ── StagingDefaults wiring ────────────────────────────────────────────────────

// TestExecutor_Dispatch_stageLocallyProceedsWithStagingDefaults verifies that
// when Config.StagingDefaults is true and staging is otherwise unconfigured
// (no ScratchDir/SyncCommand), a stage_locally assignment proceeds using the
// built-in copy + TEMP scratch rather than failing pre-exec.
func TestExecutor_Dispatch_stageLocallyProceedsWithStagingDefaults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	nc := &stubNATS{}
	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := session.NewManager(filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	cfg := executor.Config{
		KillGracePeriod: 500 * time.Millisecond,
		StagingDefaults: true,
	}
	statusPub := status.New(nc, status.Config{WorkerID: "test-worker"}, logger)
	exec := executor.New(statusPub, mgr, m, nil, cfg, logger)

	msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
	msg.PathDeliveries = []protocol.PathDelivery{{Kind: "stage_locally"}}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "succeeded" {
		t.Errorf("terminal status = %q, message = %q; want %q (StagingDefaults must let stage_locally proceed via built-in copy)",
			last.Status, last.Message, "succeeded")
	}
}

// TestExecutor_Dispatch_stageLocallyFailsWithoutStagingDefaults verifies that
// when Config.StagingDefaults is false and staging is otherwise unconfigured,
// a stage_locally assignment fails pre-exec with a message naming staging.
func TestExecutor_Dispatch_stageLocallyFailsWithoutStagingDefaults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test uses Unix-style exec")
	}

	nc := &stubNATS{}
	m := metrics.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mgr := session.NewManager(filepath.Join(t.TempDir(), "sessions"), false, isolation.NewFake(nil), workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	cfg := executor.Config{
		KillGracePeriod: 500 * time.Millisecond,
		StagingDefaults: false,
	}
	statusPub := status.New(nc, status.Config{WorkerID: "test-worker"}, logger)
	exec := executor.New(statusPub, mgr, m, nil, cfg, logger)

	msg := makeAssign("exit", map[string]string{"SQI_TEST_EXIT_CODE": "0"})
	msg.PathDeliveries = []protocol.PathDelivery{{Kind: "stage_locally"}}

	if err := exec.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	waitForStatus(t, nc, 2, 10*time.Second)

	last := nc.lastStatus()
	if last.Status != "failed" {
		t.Fatalf("terminal status = %q, want %q", last.Status, "failed")
	}
	if !strings.Contains(last.Message, "staging") {
		t.Errorf("message = %q; want it to mention staging", last.Message)
	}
}
