// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

// worker_binary_test.go — end-to-end test with a real sqi-worker subprocess.
//
// Unlike the mock-worker tests in e2e_test.go, this test compiles the real
// sqi-worker binary and runs it as a subprocess, so the full execution path
// (NATS connect, registration, lease loop, OS process execution, log streaming,
// status publishing) is exercised.
//
// The test is skipped if the sqi-worker binary cannot be built (e.g. a partial
// build environment).
//
// The binary is compiled once per test-binary run and cached via a
// package-level sync.Once, so running several tests in this file is cheap.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// packageCleanups holds teardown for resources that are shared across tests and
// therefore cannot be released by any one test's t.Cleanup. The tagged OIDC
// suite's single Keycloak container is registered here. Entries are appended
// under the same sync.Once that creates the resource, and drained by TestMain.
var packageCleanups []func()

// TestMain runs all tests in the integration package and releases what outlives
// them: the worker binary directory (cached across tests via sync.Once, so
// t.TempDir() cannot own it) and anything registered in packageCleanups.
//
// The capacity tests also run this binary as a program of their own (see
// [runStandIn]); then it does that and exits without running any test.
func TestMain(m *testing.M) {
	if code, ok := runStandIn(); ok {
		os.Exit(code)
	}
	code := m.Run()
	if workerBinaryDir != "" {
		_ = os.RemoveAll(workerBinaryDir)
	}
	for _, fn := range packageCleanups {
		fn()
	}
	os.Exit(code)
}

// ── Stand-in programs ─────────────────────────────────────────────────────────

// The ways the capacity tests (capacity_act_test.go) run this test binary as a
// program. Arguments are checked before the environment: a task inherits its
// worker's environment, which may carry standInEnv.
const (
	// standInGateArg, followed by a path, waits until that path exists and
	// exits 0: a task the test releases.
	standInGateArg = "sqi-standin-gate"
	// capScriptURLEnv makes the binary a capacity provider script: it posts
	// its standard input to the URL and prints the reply.
	capScriptURLEnv = "SQI_CAPSCRIPT_URL"
	// standInEnv set to "sleep" makes the binary a worker's sleep command: it
	// creates the file standInMarkerEnv names and exits 0.
	standInEnv       = "SQI_STANDIN"
	standInMarkerEnv = "SQI_STANDIN_MARKER"
)

// standInGateLimit bounds a gate stand-in's wait, so a test that never
// releases it does not leave the process behind.
const standInGateLimit = 10 * time.Minute

// runStandIn runs the stand-in this process was started as, if any, and
// reports its exit code.
func runStandIn() (code int, ok bool) {
	switch {
	case len(os.Args) == 3 && os.Args[1] == standInGateArg:
		return standInWaitGate(os.Args[2]), true
	case os.Getenv(capScriptURLEnv) != "":
		return standInProvider(os.Getenv(capScriptURLEnv)), true
	case os.Getenv(standInEnv) == "sleep":
		return standInSleep(os.Getenv(standInMarkerEnv)), true
	}
	return 0, false
}

// standInWaitGate waits until path exists.
func standInWaitGate(path string) int {
	deadline := time.Now().Add(standInGateLimit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "gate %s did not appear within %s\n", path, standInGateLimit)
	return 1
}

// standInProvider posts its standard input to url and prints the reply body.
func standInProvider(url string) int {
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read the request: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "build the request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "post the request: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "reply: HTTP %d, %v: %s\n", resp.StatusCode, err, reply)
		return 1
	}
	if _, err := os.Stdout.Write(reply); err != nil {
		return 1
	}
	return 0
}

// standInSleep creates the marker file at path.
func standInSleep(path string) int {
	if path == "" {
		fmt.Fprintln(os.Stderr, standInMarkerEnv+" is not set")
		return 1
	}
	if err := os.WriteFile(path, []byte("slept\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write the marker: %v\n", err)
		return 1
	}
	return 0
}

// buildWorkerBinary compiles cmd/sqi-worker into a temporary directory and
// returns the path to the executable.  The build runs once per test binary
// invocation; subsequent calls return the cached path instantly.
//
// The test is skipped, not failed, when the build cannot complete, so CI
// environments without a full Go toolchain still run the rest of the suite.
func buildWorkerBinary(tb testing.TB) string {
	tb.Helper()

	workerBinaryOnce.Do(func() {
		// Locate the repository root by walking up from this source file.
		// runtime.Caller(0) returns the source path at compile time, which is
		// valid when running tests from the source tree.
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			errWorkerBinary = errors.New("runtime.Caller(0) returned no file path")
			return
		}
		repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

		// os.MkdirTemp, not tb.TempDir: the directory must outlive the
		// individual test because the binary is cached via sync.Once for the
		// entire test-binary run. TestMain removes it.
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

		// The 3-minute timeout stops a hung build from holding the suite
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
	return startRealWorkerWithOptions(t, ts, farmID, queueID, nil, nil)
}

// startRealWorkerWithOptions is [startRealWorker] with extra global CLI args
// (placed before the "start" subcommand, e.g. "--config", path) and extra
// environment entries (appended after the standard ones), for tests that need a
// non-default worker configuration such as local staging.
func startRealWorkerWithOptions(t *testing.T, ts *testServer, farmID, queueID string, extraArgs, extraEnv []string) string {
	t.Helper()
	return startRealWorkerCore(t, ts, farmID, queueID, extraArgs, extraEnv, false)
}

// startRealWorkerAnyOS is [startRealWorker] for callers whose job commands are
// cross-platform, so the Windows skip below does not apply to them.
//
// The skip guards the job commands, not the worker: sqi-worker itself runs on
// Windows (scripts/smoke.sh drives it there), but every caller above submits a
// template whose onRun is a POSIX command such as `echo`, which has no
// equivalent on a Windows worker. A caller whose command is a real
// cross-platform binary, such as ffmpeg, is not subject to that limitation;
// skipping it on Windows would drop the only automated coverage of the
// PowerShell-gated preset, which cannot run anywhere else.
//
// The Tier-3 preset harness is the other caller, passing extraEnv to put its
// stubs on PATH: every command it runs is test/stubproc under the vendor's
// name, so no POSIX command is involved. Which presets can run on this host is
// decided per preset by unsatisfiableOSFamily, reading each template's own
// attr.worker.os.family, not by a platform check here.
func startRealWorkerAnyOS(t *testing.T, ts *testServer, farmID, queueID string, extraEnv ...string) string {
	t.Helper()
	return startRealWorkerCore(t, ts, farmID, queueID, nil, extraEnv, true)
}

// startRealWorkerCore implements the three helpers above. allowWindows selects
// whether the POSIX-command skip applies; everything else is identical.
func startRealWorkerCore(t *testing.T, ts *testServer, farmID, queueID string, extraArgs, extraEnv []string, allowWindows bool) string {
	t.Helper()
	startRealWorkerNoWait(t, ts, farmID, queueID, extraArgs, extraEnv, allowWindows)

	// Poll GET /api/v1/workers until this worker appears with status "online".
	workerID := pollForOnlineWorker(t, ts, farmID, 30*time.Second)
	t.Logf("real worker registered: id=%s", workerID)
	return workerID
}

// startRealWorkerNoWait starts a real worker subprocess and returns as soon as
// it is launched, without waiting for it to come online.
//
// A test asserting that a worker never registers needs this: for it, "never
// came online" is the expected outcome, not a fatal error.
//
// It returns the started process so a test can signal it; the cleanup below
// still stops and reaps it, and owns cmd.Wait, so callers must not call Wait.
func startRealWorkerNoWait(t *testing.T, ts *testServer, farmID, queueID string, extraArgs, extraEnv []string, allowWindows bool) *exec.Cmd {
	t.Helper()

	if runtime.GOOS == "windows" && !allowWindows {
		t.Skip("real-worker integration test requires a POSIX shell; skipping on Windows")
	}

	binaryPath := buildWorkerBinary(t)

	dataDir := t.TempDir()
	metricsPort := freePort(t)

	// Global flags (e.g. --config) precede the "start" subcommand.
	args := make([]string, 0, len(extraArgs)+1)
	args = append(args, extraArgs...)
	args = append(args, "start")

	// Use context.Background so that the subprocess lifetime is controlled by
	// t.Cleanup below rather than any caller-supplied context.
	cmd := exec.CommandContext(context.Background(), binaryPath, args...)
	cmd.Env = append(realWorkerEnv(ts, farmID, queueID, dataDir, metricsPort), extraEnv...)

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
	return cmd
}

// realWorkerEnv is the environment a real worker subprocess runs with: this
// process's own, then the settings that point it at ts, serving queueID in
// farmID from dataDir, with its metrics on metricsPort and test-friendly
// timings.
func realWorkerEnv(ts *testServer, farmID, queueID, dataDir string, metricsPort int) []string {
	return append(
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
}

// pollForOnlineWorker polls GET /api/v1/workers until any worker in farmID
// has status "online" and returns its ID, or fails the test after timeout.
func pollForOnlineWorker(t *testing.T, ts *testServer, farmID string, timeout time.Duration) string {
	t.Helper()
	id := findOnlineWorker(t, ts, farmID, timeout)
	if id == "" {
		t.Fatalf("pollForOnlineWorker: no online worker in farm %s within %s", farmID, timeout)
	}
	return id
}

// findOnlineWorker is [pollForOnlineWorker] without the fatal: it returns ""
// on timeout. A test asserting that a worker never registers needs that, since
// for it a timeout is the expected result.
func findOnlineWorker(t *testing.T, ts *testServer, farmID string, timeout time.Duration) string {
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
			apiURL(ts, "/api/v1/workers"), nil,
		)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		r, err := clientFor(ts).Do(req)
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

	return ""
}

// ── Job YAML for real execution ───────────────────────────────────────────────

// workerBinaryJobYAML is an OpenJD job with a real echo command that the
// worker subprocess executes. TestWorkerBinaryEndToEnd asserts its output
// ("hello from real sqi-worker") in the task's logs.
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

	// Wait for the job to reach a terminal state.
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
