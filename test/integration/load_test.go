// SPDX-License-Identifier: AGPL-3.0-only

package integration

// load_test.go — scheduler throughput and assignment-latency load test.
//
// This file adds two entry points:
//
//   - TestLoadScheduler: a table-driven test with configurable worker/job
//     counts, suitable for CI characterisation runs.  Override scale via
//     environment variables SQI_LOAD_WORKERS and SQI_LOAD_JOBS.
//
//   - BenchmarkSchedulerAssignment: a Go benchmark (b.N iterations, each a
//     fresh server) so `go test -bench` can compare scheduler throughput
//     across revisions.
//
// Both entry points boot a full sqi-server (SQLite + embedded NATS +
// scheduler + HTTP), register N mock workers, submit M jobs concurrently,
// and drain completions while measuring:
//
//   - Assignment latency per job: wall time from REST submit → worker
//     receives the first assignment message for that job.
//   - Total elapsed time from first submit to last REST confirmation.
//   - Throughput: completed jobs / elapsed second.
//
// These are characterisation metrics, not correctness assertions.  The only
// hard assertion is that every submitted job eventually reaches a terminal
// state within the (generous) deadline.
//
// # Running
//
//	# default scenarios
//	go test -v -run TestLoadScheduler ./test/integration/
//
//	# custom scale
//	SQI_LOAD_WORKERS=20 SQI_LOAD_JOBS=200 \
//	  go test -v -run TestLoadScheduler -timeout=5m ./test/integration/
//
//	# benchmark (3 iterations)
//	go test -bench=BenchmarkSchedulerAssignment -benchtime=3x \
//	  -timeout=15m ./test/integration/

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Scenario configuration ────────────────────────────────────────────────────

// loadScenario describes a single load run.
type loadScenario struct {
	workers int // concurrent mock workers
	jobs    int // jobs submitted
}

// defaultLoadScenarios are exercised by TestLoadScheduler.  They are
// intentionally small so the suite completes in under two minutes on CI.
var defaultLoadScenarios = []loadScenario{
	{workers: 2, jobs: 10},
	{workers: 5, jobs: 25},
	{workers: 10, jobs: 50},
}

// envInt returns the integer value of an environment variable, or fallback if
// the variable is unset, empty, or not a positive integer.
func envInt(name string, fallback int) int {
	s := os.Getenv(name)
	if s == "" {
		return fallback
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 {
		return fallback
	}
	return v
}

// ── Load-test server ──────────────────────────────────────────────────────────

// startLoadServer boots a full sqi-server tuned for throughput (tight assign
// interval, larger batch, more assign goroutines) and returns once the server
// passes /readyz.  Cleanup is registered on tb.
//
// It reuses freePort / waitForTCP / waitForReadyz from harness_test.go, which
// were widened to accept testing.TB.
func startLoadServer(tb testing.TB) *testServer {
	tb.Helper()

	httpPort := freePort(tb)
	natsPort := freePort(tb)

	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	natsAddr := fmt.Sprintf("127.0.0.1:%d", natsPort)

	tmpDir := tb.TempDir()
	sqlitePath := tmpDir + "/sqi-load.db"
	natsDir := tmpDir + "/nats"

	cfg := server.Config{
		HTTPAddr:           httpAddr,
		CORSOrigins:        []string{"*"},
		EnablePprof:        false,
		NATSAddr:           natsAddr,
		NATSDataDir:        natsDir,
		NATSMaxStoreMB:     128,
		SQLitePath:         sqlitePath,
		CheckpointInterval: time.Minute,
		Scheduler: scheduler.Config{
			AssignInterval:         50 * time.Millisecond,
			AssignBatchSize:        50,
			AssignWorkers:          4,
			WorkerTimeout:          60 * time.Second,
			HeartbeatSweepInterval: 30 * time.Second,
		},
		DiscoveryEnabled: false,
		DisableRateLimit: true,
	}

	logger := slog.New(slog.DiscardHandler)
	srv := server.New(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Detect immediate startup failure before polling TCP.
	select {
	case err := <-done:
		cancel()
		tb.Fatalf("startLoadServer: server exited during startup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if !waitForTCP(tb, httpAddr, 15*time.Second) {
		cancel()
		tb.Fatalf("startLoadServer: HTTP not up on %s within 15s", httpAddr)
	}
	if !waitForReadyz(tb, httpAddr, 15*time.Second) {
		cancel()
		tb.Fatalf("startLoadServer: /readyz not ready within 15s")
	}

	ts := &testServer{
		HTTPAddr: httpAddr,
		NATSAddr: natsAddr,
		cancel:   cancel,
		done:     done,
	}
	tb.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			tb.Logf("warning: load server did not stop within 20s")
		}
	})
	return ts
}

// ── Load worker ───────────────────────────────────────────────────────────────

// loadWorker is a concurrent mock worker for load testing: it registers once,
// runs a heartbeat goroutine, and drains task assignments as fast as possible
// by immediately publishing running → succeeded.
type loadWorker struct {
	id       string
	farmID   string
	queueID  string
	nc       *nats.Conn
	js       jetstream.JetStream
	consumer jetstream.Consumer
}

// newLoadWorker dials NATS and attaches to the shared durable pull consumer
// for queueID.  Multiple load workers share the same durable consumer and
// naturally round-robin assignments among themselves — matching real worker
// pull semantics.
func newLoadWorker(tb testing.TB, natsURL, workerID, farmID, queueID string) *loadWorker {
	tb.Helper()

	nc, err := nats.Connect(
		natsURL,
		nats.MaxReconnects(10),
		nats.ReconnectWait(200*time.Millisecond),
	)
	if err != nil {
		tb.Fatalf("newLoadWorker %s: nats.Connect: %v", workerID, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		tb.Fatalf("newLoadWorker %s: jetstream.New: %v", workerID, err)
	}

	consumer, err := js.CreateOrUpdateConsumer(context.Background(), bus.StreamWork, jetstream.ConsumerConfig{
		Durable:       "sqi-work-" + sanitize(queueID),
		FilterSubject: bus.WorkAssignSubject(queueID),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		Description:   "load test pull consumer for queue " + queueID,
	})
	if err != nil {
		nc.Close()
		tb.Fatalf("newLoadWorker %s: create consumer: %v", workerID, err)
	}

	tb.Cleanup(func() {
		if !nc.IsClosed() {
			nc.Close()
		}
	})

	return &loadWorker{
		id:       workerID,
		farmID:   farmID,
		queueID:  queueID,
		nc:       nc,
		js:       js,
		consumer: consumer,
	}
}

// register publishes a RegisterMsg so the server records this worker as online.
func (w *loadWorker) register(tb testing.TB) {
	tb.Helper()
	msg := protocol.RegisterMsg{
		Version:            protocol.ProtocolVersion,
		Type:               protocol.TypeRegister,
		WorkerID:           w.id,
		FarmID:             w.farmID,
		QueueID:            w.queueID,
		Hostname:           "load-worker-" + w.id,
		OS:                 "linux",
		CPUCount:           8,
		RAMMb:              16384,
		MaxConcurrentTasks: 4,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		tb.Fatalf("loadWorker.register %s: marshal: %v", w.id, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.js.Publish(ctx, bus.SubjectWorkerRegister, data); err != nil {
		tb.Fatalf("loadWorker.register %s: publish: %v", w.id, err)
	}
}

// heartbeatLoop sends heartbeats at interval until ctx is done, then signals
// wg.Done.
func (w *loadWorker) heartbeatLoop(ctx context.Context, interval time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msg := protocol.HeartbeatMsg{
				Version:  protocol.ProtocolVersion,
				Type:     protocol.TypeHeartbeat,
				WorkerID: w.id,
				At:       time.Now().UTC(),
			}
			data, err := json.Marshal(msg)
			if err != nil {
				return
			}
			pubCtx, pubCancel := context.WithTimeout(ctx, 2*time.Second)
			if _, pubErr := w.js.Publish(pubCtx, bus.SubjectWorkerHeartbeat, data); pubErr != nil {
				pubCancel()
				return
			}
			pubCancel()
		}
	}
}

// drainLoop pulls assignments and immediately completes them (running →
// succeeded).  For each assignment it calls assignedAt(jobID, receivedAt).
// Runs until ctx is canceled, then signals wg.Done.
//
// assignedAt is called with the first recorded time for each job; the guard
// prevents double-counting if the scheduler redelivers a message.
func (w *loadWorker) drainLoop(ctx context.Context, wg *sync.WaitGroup, assignedAt func(jobID string, at time.Time)) {
	defer wg.Done()
	exitCode := 0
	for {
		if ctx.Err() != nil {
			return
		}
		msgs, err := w.consumer.Fetch(4, jetstream.FetchMaxWait(300*time.Millisecond))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Transient fetch error (e.g. window expiry) — retry.
			continue
		}
		for msg := range msgs.Messages() {
			now := time.Now()
			var assign protocol.AssignMsg
			if err := json.Unmarshal(msg.Data(), &assign); err != nil {
				if nakErr := msg.Nak(); nakErr != nil {
					// Non-fatal; redelivery will handle it.
					_ = nakErr
				}
				continue
			}
			if err := msg.Ack(); err != nil {
				// Non-fatal during teardown.
				continue
			}
			if assignedAt != nil {
				assignedAt(assign.JobID, now)
			}
			w.publishStatus(assign, "running", nil)
			w.publishStatus(assign, "succeeded", &exitCode)
		}
		// Log batch-level errors so stalls are diagnosable, but keep draining.
		if batchErr := msgs.Error(); batchErr != nil &&
			!errors.Is(batchErr, context.DeadlineExceeded) &&
			!errors.Is(batchErr, nats.ErrTimeout) {
			// Best-effort — we cannot call tb.Log here since drainLoop runs in
			// a goroutine started after the test may have ended.  The error
			// will surface as a timeout in waitAllJobsTerminal.
			_ = batchErr
		}
	}
}

// publishStatus publishes a TaskStatusMsg to task.status.<jobID>.
func (w *loadWorker) publishStatus(assign protocol.AssignMsg, status string, exitCode *int) {
	msg := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    assign.TaskID,
		AttemptID: assign.AttemptID,
		JobID:     assign.JobID,
		Status:    status,
		ExitCode:  exitCode,
		SessionID: "load-" + assign.TaskID,
		At:        time.Now().UTC(),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := w.js.Publish(ctx, bus.TaskStatusSubject(assign.JobID), data); err != nil {
		// Best-effort in load loops; callers detect missed completions via
		// the waitAllJobsTerminal timeout.
		_ = err
	}
}

// ── Latency statistics ────────────────────────────────────────────────────────

// latencyStats holds basic percentile statistics for a sample set.
type latencyStats struct {
	Count int
	Min   time.Duration
	Max   time.Duration
	Mean  time.Duration
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
}

func computeStats(samples []time.Duration) latencyStats {
	if len(samples) == 0 {
		return latencyStats{}
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	slices.Sort(sorted)

	var sum int64
	for _, d := range sorted {
		sum += int64(d)
	}

	pct := func(p float64) time.Duration {
		idx := max(int(math.Ceil(p/100.0*float64(len(sorted))))-1, 0)
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}

	return latencyStats{
		Count: len(sorted),
		Min:   sorted[0],
		Max:   sorted[len(sorted)-1],
		Mean:  time.Duration(sum / int64(len(sorted))),
		P50:   pct(50),
		P95:   pct(95),
		P99:   pct(99),
	}
}

// ── REST helpers ──────────────────────────────────────────────────────────────

// loadAPIURL builds a full URL against ts.
func loadAPIURL(ts *testServer, path string) string {
	return "http://" + ts.HTTPAddr + path
}

// loadDoJSON performs an HTTP request and optionally decodes the JSON response.
func loadDoJSON(tb testing.TB, method, url string, body []byte, contentType string, expectStatus int, dst any) {
	tb.Helper()
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		tb.Fatalf("loadDoJSON: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		tb.Fatalf("loadDoJSON: %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != expectStatus {
		var buf bytes.Buffer
		if _, readErr := buf.ReadFrom(resp.Body); readErr != nil {
			tb.Logf("loadDoJSON: read error body: %v", readErr)
		}
		tb.Fatalf("loadDoJSON: %s %s: got %d, want %d\nbody: %s",
			method, url, resp.StatusCode, expectStatus, buf.String())
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			tb.Fatalf("loadDoJSON: decode: %v", err)
		}
	}
}

// seedLoadFarmAndQueue creates a farm and queue for a load test run.
func seedLoadFarmAndQueue(tb testing.TB, ts *testServer) (farmID, queueID string) {
	tb.Helper()

	farmBody := []byte(`{"name":"Load Farm"}`)
	var farmResp struct {
		ID string `json:"id"`
	}
	loadDoJSON(tb, http.MethodPost, loadAPIURL(ts, "/api/v1/farms"), farmBody, "application/json", http.StatusCreated, &farmResp)
	if farmResp.ID == "" {
		tb.Fatal("seedLoadFarmAndQueue: empty farm ID")
	}

	queueBody, err := json.Marshal(map[string]any{"farm_id": farmResp.ID, "name": "Load Queue"})
	if err != nil {
		tb.Fatalf("seedLoadFarmAndQueue: marshal queue: %v", err)
	}
	var queueResp struct {
		ID string `json:"id"`
	}
	loadDoJSON(tb, http.MethodPost, loadAPIURL(ts, "/api/v1/queues"), queueBody, "application/json", http.StatusCreated, &queueResp)
	if queueResp.ID == "" {
		tb.Fatal("seedLoadFarmAndQueue: empty queue ID")
	}
	return farmResp.ID, queueResp.ID
}

// submitLoadJob submits a single job and returns its ID.  The submit time
// (recorded *before* the HTTP call to avoid systematic latency bias) is
// returned alongside the ID.
func submitLoadJob(tb testing.TB, ts *testServer, farmID, queueID, owner string) (jobID string, submitAt time.Time) {
	tb.Helper()
	u := fmt.Sprintf("%s?farm_id=%s&queue_id=%s&owner=%s",
		loadAPIURL(ts, "/api/v1/jobs"), farmID, queueID, owner)
	submitAt = time.Now() // capture before the round-trip
	var resp jobResp
	loadDoJSON(tb, http.MethodPost, u, []byte(minimalJobYAML), "application/x-yaml", http.StatusCreated, &resp)
	if resp.ID == "" {
		tb.Fatal("submitLoadJob: empty job ID")
	}
	return resp.ID, submitAt
}

// pollAllWorkersOnline polls GET /api/v1/workers until every worker in lws
// shows status "online", or timeout elapses.
func pollAllWorkersOnline(tb testing.TB, ts *testServer, lws []*loadWorker, timeout time.Duration) {
	tb.Helper()
	wantIDs := make(map[string]bool, len(lws))
	for _, lw := range lws {
		wantIDs[lw.id] = true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var resp workerListResp
		loadDoJSON(tb, http.MethodGet, loadAPIURL(ts, "/api/v1/workers"), nil, "", http.StatusOK, &resp)
		online := make(map[string]bool, len(resp.Items))
		for _, w := range resp.Items {
			if w.Status == "online" {
				online[w.ID] = true
			}
		}
		allOnline := true
		for id := range wantIDs {
			if !online[id] {
				allOnline = false
				break
			}
		}
		if allOnline {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	tb.Fatalf("pollAllWorkersOnline: not all workers online within %s", timeout)
}

// waitAllJobsTerminal polls until every job in jobIDs has reached a terminal
// state ("completed", "failed", or "canceled"), or timeout elapses.
func waitAllJobsTerminal(tb testing.TB, ts *testServer, jobIDs []string, timeout time.Duration) {
	tb.Helper()
	terminal := map[string]bool{"completed": true, "failed": true, "canceled": true}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		done := true
		for _, jobID := range jobIDs {
			var resp jobResp
			loadDoJSON(tb, http.MethodGet, loadAPIURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &resp)
			if !terminal[resp.Status] {
				done = false
				break
			}
		}
		if done {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	tb.Fatalf("waitAllJobsTerminal: not all jobs reached a terminal state within %s", timeout)
}

// ── Core load run ─────────────────────────────────────────────────────────────

// runLoad executes sc against ts and returns per-job assignment-latency
// samples and total elapsed time.  Both *testing.T and *testing.B are
// accepted via testing.TB.
//
// Goroutine lifecycle: all per-worker goroutines (heartbeat, drain) are
// tracked in a sync.WaitGroup; workerCancel is called before runLoad
// returns and the function blocks until all goroutines have exited.  This
// prevents goroutine leaks and NATS-after-close panics between benchmark
// iterations.
func runLoad(tb testing.TB, ts *testServer, sc loadScenario) (samples []time.Duration, elapsed time.Duration) {
	tb.Helper()

	natsURL := "nats://" + ts.NATSAddr
	farmID, queueID := seedLoadFarmAndQueue(tb, ts)

	// Create and register N workers.
	workers := make([]*loadWorker, sc.workers)
	for i := range sc.workers {
		id := fmt.Sprintf("lw-%03d", i)
		lw := newLoadWorker(tb, natsURL, id, farmID, queueID)
		lw.register(tb)
		workers[i] = lw
	}
	pollAllWorkersOnline(tb, ts, workers, 20*time.Second)

	// Start worker goroutines, all tracked in wg so we can join them cleanly.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	var workerWg sync.WaitGroup

	var mu sync.Mutex
	assignTimes := make(map[string]time.Time, sc.jobs)

	for _, lw := range workers {
		workerWg.Add(2)
		go lw.heartbeatLoop(workerCtx, 5*time.Second, &workerWg)
		go lw.drainLoop(workerCtx, &workerWg, func(jobID string, at time.Time) {
			mu.Lock()
			// Guard: record only the first arrival; scheduler may redeliver.
			if _, seen := assignTimes[jobID]; !seen {
				assignTimes[jobID] = at
			}
			mu.Unlock()
		})
	}

	// Submit M jobs in parallel; record submit time *before* the HTTP call
	// to avoid systematic latency bias (server creates the job before
	// returning, so time-after-response understates real scheduler latency).
	submitTimes := make(map[string]time.Time, sc.jobs)
	var submitMu sync.Mutex
	var wg sync.WaitGroup

	submitStart := time.Now()

	for i := range sc.jobs {
		wg.Go(func() {
			owner := fmt.Sprintf("load-job-%04d", i)
			jobID, submitAt := submitLoadJob(tb, ts, farmID, queueID, owner)
			submitMu.Lock()
			submitTimes[jobID] = submitAt
			submitMu.Unlock()
		})
	}
	wg.Wait()

	// Collect submitted job IDs.
	submitMu.Lock()
	jobIDs := make([]string, 0, len(submitTimes))
	for id := range submitTimes {
		jobIDs = append(jobIDs, id)
	}
	submitMu.Unlock()

	// Wait for all jobs to reach a terminal REST state.
	waitAllJobsTerminal(tb, ts, jobIDs, 120*time.Second)
	elapsed = time.Since(submitStart)

	// Stop worker goroutines and wait for them to exit before returning,
	// preventing NATS-after-close panics in benchmark iterations.
	workerCancel()
	workerWg.Wait()

	// Compute latency samples: submit → first assignment received.
	submitMu.Lock()
	mu.Lock()
	for jobID, st := range submitTimes {
		if at, ok := assignTimes[jobID]; ok {
			samples = append(samples, at.Sub(st))
		}
	}
	mu.Unlock()
	submitMu.Unlock()

	return samples, elapsed
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestLoadScheduler runs defaultLoadScenarios (or a single scenario from
// SQI_LOAD_WORKERS / SQI_LOAD_JOBS) and reports throughput and latency stats.
// It fails only if jobs do not complete within the generous per-scenario deadline.
func TestLoadScheduler(t *testing.T) {
	scenarios := defaultLoadScenarios
	ew := envInt("SQI_LOAD_WORKERS", 0)
	ej := envInt("SQI_LOAD_JOBS", 0)
	if ew > 0 && ej > 0 {
		scenarios = []loadScenario{{workers: ew, jobs: ej}}
	}

	for _, sc := range scenarios {
		name := fmt.Sprintf("workers=%d/jobs=%d", sc.workers, sc.jobs)
		t.Run(name, func(t *testing.T) {
			ts := startLoadServer(t)
			samples, elapsed := runLoad(t, ts, sc)

			stats := computeStats(samples)
			throughput := float64(sc.jobs) / elapsed.Seconds()

			t.Logf("=== Load Results: %s ===", name)
			t.Logf("  Elapsed:           %s", elapsed.Round(time.Millisecond))
			t.Logf("  Jobs sampled:      %d / %d", stats.Count, sc.jobs)
			t.Logf("  Throughput:        %.2f jobs/sec", throughput)
			if stats.Count > 0 {
				t.Logf("  Assignment latency:")
				t.Logf("    min:  %s", stats.Min.Round(time.Millisecond))
				t.Logf("    mean: %s", stats.Mean.Round(time.Millisecond))
				t.Logf("    p50:  %s", stats.P50.Round(time.Millisecond))
				t.Logf("    p95:  %s", stats.P95.Round(time.Millisecond))
				t.Logf("    p99:  %s", stats.P99.Round(time.Millisecond))
				t.Logf("    max:  %s", stats.Max.Round(time.Millisecond))
			}

			if stats.Count < sc.jobs {
				t.Errorf("only %d/%d jobs received recorded assignments; scheduler may have dropped work",
					stats.Count, sc.jobs)
			}
		})
	}
}

// BenchmarkSchedulerAssignment measures scheduler throughput for a fixed
// workers=5 / jobs=20 scenario.  b.N controls complete iterations (each with
// a fresh server boot), suitable for revision-to-revision comparisons.
//
//	go test -bench=BenchmarkSchedulerAssignment -benchtime=3x -timeout=15m ./test/integration/
func BenchmarkSchedulerAssignment(b *testing.B) {
	const (
		benchWorkers = 5
		benchJobs    = 20
	)

	b.ReportAllocs()
	sc := loadScenario{workers: benchWorkers, jobs: benchJobs}

	for range b.N {
		b.StopTimer()
		ts := startLoadServer(b)
		b.StartTimer()

		samples, elapsed := runLoad(b, ts, sc)

		b.StopTimer()

		stats := computeStats(samples)
		throughput := float64(benchJobs) / elapsed.Seconds()

		b.ReportMetric(throughput, "jobs/sec")
		b.ReportMetric(float64(stats.P50.Milliseconds()), "p50-assign-ms")
		b.ReportMetric(float64(stats.P95.Milliseconds()), "p95-assign-ms")
		// Timer remains stopped; b.N loop will call b.StopTimer again at the
		// start of the next iteration (during server setup), which is a no-op
		// on an already-stopped timer.
	}
}
