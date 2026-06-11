// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// worker_binary_test.go — end-to-end test with a real sqi-worker subprocess.
//
// Unlike the mock-worker tests in e2e_test.go, this test compiles the real
// sqi-worker binary and runs it as a subprocess so the full execution path
// (NATS connect, registration, pull loop, OS process execution, log streaming,
// status publishing) is exercised from first principles.
//
// The test is automatically skipped if the sqi-worker binary cannot be built
// (e.g. partial build environment), so it never blocks the overall suite.
//
// Build cost: the binary is compiled once per test-binary run and cached via
// a package-level sync.Once, so running multiple tests in this file is cheap.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Binary build cache ────────────────────────────────────────────────────────

var (
	workerBinaryOnce sync.Once
	workerBinaryPath string
	workerBinaryDir  string // removed by TestMain after all tests complete
	errWorkerBinary  error
)

// TestMain runs all tests in the integration package and removes the worker
// binary directory afterwards.  The directory cannot be cleaned up via
// t.TempDir() because the binary is cached across multiple tests via
// sync.Once; TestMain is the only reliable cleanup hook.
func TestMain(m *testing.M) {
	code := m.Run()
	if workerBinaryDir != "" {
		_ = os.RemoveAll(workerBinaryDir)
	}
	os.Exit(code)
}

// buildWorkerBinary compiles cmd/sqi-worker into a temporary directory and
// returns the path to the executable.  The build runs once per test binary
// invocation; subsequent calls return the cached path instantly.
//
// The test is skipped (not failed) when the build cannot complete so that CI
// environments without a full Go toolchain do not block the suite.
func buildWorkerBinary(tb testing.TB) string {
	tb.Helper()

	workerBinaryOnce.Do(func() {
		// Locate the repository root by walking up from this source file.
		// runtime.Caller(0) returns the source path at compile time, which is
		// always valid when running tests from the source tree.
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			errWorkerBinary = errors.New("runtime.Caller(0) returned no file path")
			return
		}
		repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

		// os.MkdirTemp (not tb.TempDir) is intentional: the directory must
		// outlive the individual test because the binary is cached via
		// sync.Once for the entire test-binary run.  TestMain removes it.
		tmpDir, err := os.MkdirTemp("", "sqi-worker-binary-*") //nolint:usetesting // must outlive the calling test; cached via sync.Once; removed by TestMain
		if err != nil {
			errWorkerBinary = fmt.Errorf("create temp dir for worker binary: %w", err)
			return
		}
		workerBinaryDir = tmpDir

		name := "sqi-worker"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		out := filepath.Join(tmpDir, name)

		// A 3-minute timeout prevents a hung build from blocking the suite
		// until the outer go test -timeout fires.
		buildCtx, buildCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(buildCtx, "go", "build", "-o", out, "./cmd/sqi-worker")
		cmd.Dir = repoRoot
		output, buildErr := cmd.CombinedOutput()
		buildCancel()
		if buildErr != nil {
			errWorkerBinary = fmt.Errorf("go build ./cmd/sqi-worker: %w\n%s", buildErr, output)
			return
		}
		workerBinaryPath = out
	})

	if errWorkerBinary != nil {
		tb.Skipf("skipping real-worker test: sqi-worker binary unavailable: %v", errWorkerBinary)
	}
	return workerBinaryPath
}

// ── Worker process helper ─────────────────────────────────────────────────────

// startRealWorker launches a sqi-worker subprocess configured to connect to ts
// and serve tasks from queueID in farmID.  It blocks until the worker appears
// online in GET /api/v1/workers and returns its worker ID.
//
// The process is automatically terminated via t.Cleanup.  Logs from the
// subprocess are forwarded to t.Log for visibility in -v mode.
func startRealWorker(t *testing.T, ts *testServer, farmID, queueID string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("real-worker integration test requires a POSIX echo command; skipping on Windows")
	}

	binaryPath := buildWorkerBinary(t)

	dataDir := t.TempDir()
	metricsPort := freePort(t)

	// Use context.Background so that the subprocess lifetime is controlled by
	// t.Cleanup below rather than any caller-supplied context.
	cmd := exec.CommandContext(context.Background(), binaryPath, "start")
	cmd.Env = append(
		os.Environ(),
		"SQI_WORKER_NATS_URL=nats://"+ts.NATSAddr,
		"SQI_WORKER_DISCOVERY_ENABLE_MDNS=false",
		"SQI_WORKER_FARM_ID="+farmID,
		"SQI_WORKER_QUEUE_IDS="+queueID,
		"SQI_WORKER_DATA_DIR="+dataDir,
		"SQI_WORKER_ALLOW_ROOT=true",
		"SQI_WORKER_LOG_FORMAT=text",
		"SQI_WORKER_LOG_LEVEL=warn",
		"SQI_WORKER_HEARTBEAT_INTERVAL=1s",
		"SQI_WORKER_SHUTDOWN_GRACE_PERIOD=5s",
		"SQI_WORKER_PULL_IDLE_BACKOFF=200ms",
		fmt.Sprintf("SQI_WORKER_METRICS_ADDR=127.0.0.1:%d", metricsPort),
	)
	// Route subprocess stdout/stderr to test stderr so output appears in
	// `go test -v` and in CI logs when the test fails.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("startRealWorker: start subprocess: %v", err)
	}

	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		// A single goroutine owns cmd.Wait to avoid a data race between
		// concurrent Wait callers on the timeout path.
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = cmd.Wait() //nolint:errcheck // best-effort drain; process may have exited already
		}()
		// Graceful SIGINT first; give the worker time to drain and deregister.
		_ = cmd.Process.Signal(os.Interrupt) //nolint:errcheck // best-effort; process may have already exited
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill() //nolint:errcheck // best-effort force-kill
			<-done                 // join the Wait goroutine; never call Wait again
		}
	})

	// Poll GET /api/v1/workers until this worker appears with status "online".
	// The worker needs to connect to NATS, register, and receive at least one
	// heartbeat sweep before the server marks it online.
	workerID := pollForOnlineWorker(t, ts, farmID, 30*time.Second)
	t.Logf("real worker registered: id=%s", workerID)
	return workerID
}

// pollForOnlineWorker polls GET /api/v1/workers until any worker in farmID
// has status "online" and returns its ID, or fails the test after timeout.
func pollForOnlineWorker(t *testing.T, ts *testServer, farmID string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var resp struct {
			Items []struct {
				ID     string `json:"id"`
				FarmID string `json:"farm_id"`
				Status string `json:"status"`
			} `json:"items"`
			Total int `json:"total"`
		}

		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodGet,
			"http://"+ts.HTTPAddr+"/api/v1/workers", nil,
		)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		r, err := httpClient.Do(req)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
			r.Body.Close()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		r.Body.Close()

		for _, w := range resp.Items {
			if w.FarmID == farmID && w.Status == "online" {
				return w.ID
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("pollForOnlineWorker: no online worker in farm %s within %s", farmID, timeout)
	return ""
}

// ── Job YAML for real execution ───────────────────────────────────────────────

// workerBinaryJobYAML is an OpenJD job with a real echo command that the
// worker subprocess will actually execute.  The output ("hello from real
// sqi-worker") is asserted in the log assertion step below.
const workerBinaryJobYAML = `specificationVersion: "jobtemplate-2023-09"
name: Real Worker Integration Test

steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args:
            - "hello from real sqi-worker"
`

// ── Test ──────────────────────────────────────────────────────────────────────

// TestWorkerBinaryEndToEnd boots a full sqi-server, starts a real sqi-worker
// subprocess, submits a minimal OpenJD job, and asserts:
//
//   - The worker registers and appears online via GET /api/v1/workers.
//   - The task is assigned to the worker and executed (real OS process).
//   - Log output from the process is streamed to the server.
//   - The job reaches "completed" status with correct timing data.
func TestWorkerBinaryEndToEnd(t *testing.T) {
	ts := startServer(t)

	farmID, queueID := seedFarmAndQueue(t, ts)

	// Start the real worker binary and wait for it to register.
	workerID := startRealWorker(t, ts, farmID, queueID)

	// Submit a job that the worker will execute for real.
	url := fmt.Sprintf("http://%s/api/v1/jobs?farm_id=%s&queue_id=%s&owner=real-worker-test",
		ts.HTTPAddr, farmID, queueID)
	var jobResp struct {
		ID string `json:"id"`
	}
	mustDoJSON(t, http.MethodPost, url,
		[]byte(workerBinaryJobYAML), "application/x-yaml",
		http.StatusCreated, &jobResp)
	if jobResp.ID == "" {
		t.Fatal("server returned empty job ID")
	}
	t.Logf("submitted job %s to farm %s queue %s", jobResp.ID, farmID, queueID)

	// Wait for the job to reach a terminal state.  The real worker executes
	// "echo hello from real sqi-worker" which should complete quickly.
	finalStatus := pollJobStatus(t, ts, jobResp.ID,
		[]string{"completed", "failed", "canceled"}, 60*time.Second)
	if finalStatus != "completed" {
		t.Errorf("job final status: got %q, want %q", finalStatus, "completed")
	}

	// Verify log output was streamed to the server.
	taskID := firstTaskID(t, ts, jobResp.ID)
	logs := pollTaskLogs(t, ts, taskID, "hello from real sqi-worker", 15*time.Second)
	if !strings.Contains(logs, "hello from real sqi-worker") {
		t.Errorf("expected log output to contain %q; got: %q",
			"hello from real sqi-worker", logs)
	}

	// Assert that the task was assigned to the real worker with timing data.
	// The task response includes assigned_worker_id and assigned_at, which the
	// server records when the scheduler dispatches to the real worker.
	var taskDetail struct {
		Status           string  `json:"status"`
		AssignedWorkerID string  `json:"assigned_worker_id"`
		AssignedAt       *string `json:"assigned_at"`
	}
	mustDoJSON(t, http.MethodGet,
		"http://"+ts.HTTPAddr+"/api/v1/tasks/"+taskID, nil, "",
		http.StatusOK, &taskDetail)

	if taskDetail.AssignedWorkerID != workerID {
		t.Errorf("task.assigned_worker_id: got %q, want %q",
			taskDetail.AssignedWorkerID, workerID)
	}
	if taskDetail.AssignedAt == nil {
		t.Error("task.assigned_at is nil; expected server-recorded assignment time")
	}
	if taskDetail.Status != "succeeded" {
		t.Errorf("task.status: got %q, want %q", taskDetail.Status, "succeeded")
	}

	t.Logf("worker %s completed job %s successfully", workerID, jobResp.ID)
}
