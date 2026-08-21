// SPDX-License-Identifier: AGPL-3.0-or-later

// Package integration provides an end-to-end test harness for sqi-server.
//
// The harness boots a full [server.Server] (SQLite on a temp file, embedded
// NATS, scheduler, HTTP) inside the test process, then drives a mock worker
// over NATS to verify the complete job lifecycle:
//
//	submit job (REST) → lease (NATS request/reply) → log streaming (NATS) →
//	completion (NATS status) → job succeeded (REST poll)
//
// Integration tests live here rather than alongside a single package because
// they exercise the interaction between server, bus, scheduler, store, and
// API components simultaneously.  Unit tests for individual packages continue
// to live next to the code they cover.
//
// # Build tag
//
// These tests are not tagged; they run with the normal `go test ./...` invocation.
// They do allocate OS-level TCP ports and filesystem temp directories, so they
// are skipped in environments where the NATS server cannot bind (detected via a
// timeout on startup).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Port helpers ──────────────────────────────────────────────────────────────

// freePort asks the OS for an available TCP port on loopback and returns it.
// The port is released immediately after being discovered, so there is a
// tiny window where another process could claim it.  In practice this is
// acceptable for integration tests on CI where ports are not heavily contested.
func freePort(tb testing.TB) int {
	tb.Helper()
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("freePort: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if err := ln.Close(); err != nil {
		tb.Fatalf("freePort: close listener: %v", err)
	}
	if !ok {
		tb.Fatalf("freePort: unexpected address type %T", ln.Addr())
	}
	return tcpAddr.Port
}

// ── Server harness ────────────────────────────────────────────────────────────

// testServer holds the runtime state of a server instance started for a test.
type testServer struct {
	// HTTPAddr is the full "host:port" address the HTTP server is listening on.
	HTTPAddr string
	// NATSAddr is the full "host:port" address the embedded NATS server
	// is listening on.  Workers connect to "nats://" + NATSAddr.
	NATSAddr string

	cancel context.CancelFunc
	done   chan error
}

// startServer boots a full sqi-server with a temp SQLite file, temp NATS data
// directory, and scheduler tuned for fast test cycles.  It blocks until the
// HTTP server is accepting connections and /readyz returns 200 (verifying NATS
// is up and streams are provisioned), then returns a *testServer.
//
// The server is stopped automatically via t.Cleanup.
func startServer(t *testing.T) *testServer {
	t.Helper()

	httpPort := freePort(t)
	natsPort := freePort(t)

	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	natsAddr := fmt.Sprintf("127.0.0.1:%d", natsPort)

	// Temp directories for SQLite and NATS JetStream storage.
	tmpDir := t.TempDir()
	sqlitePath := tmpDir + "/sqi-test.db"
	natsDir := tmpDir + "/nats"

	cfg := server.Config{
		HTTPAddr:           httpAddr,
		CORSOrigins:        []string{"*"},
		EnablePprof:        false,
		NATSAddr:           natsAddr,
		NATSDataDir:        natsDir,
		NATSMaxStoreMB:     64,
		SQLitePath:         sqlitePath,
		CheckpointInterval: time.Minute,
		// Tight scheduler timing so tests don't wait a full second per poll.
		Scheduler: scheduler.Config{
			AssignInterval:         100 * time.Millisecond,
			AssignBatchSize:        10,
			AssignWorkers:          2,
			WorkerTimeout:          30 * time.Second,
			HeartbeatSweepInterval: 15 * time.Second,
		},
		// mDNS disabled: multicast is not available in most CI environments.
		DiscoveryEnabled: false,
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn, // suppress info noise in test output
	}))

	srv := server.New(cfg, logger, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
	}()

	// Detect early startup failure (e.g. port conflict, SQLite error) before
	// spending time polling TCP.
	select {
	case err := <-done:
		cancel()
		t.Fatalf("startServer: server exited during startup: %v", err)
	case <-time.After(50 * time.Millisecond):
		// Still running — proceed to TCP wait.
	}

	// Wait until the HTTP server is ready to accept connections.
	if !waitForTCP(t, httpAddr, 10*time.Second) {
		cancel()
		t.Fatalf("startServer: HTTP server did not come up on %s within 10s", httpAddr)
	}

	// Poll /readyz (which gates on NATS and SQLite being healthy) rather than
	// sleeping a fixed duration.  This ensures JetStream streams are fully
	// provisioned before the test starts publishing.
	if !waitForReadyz(t, httpAddr, 10*time.Second) {
		cancel()
		t.Fatalf("startServer: server did not become ready (/readyz) within 10s")
	}

	ts := &testServer{
		HTTPAddr: httpAddr,
		NATSAddr: natsAddr,
		cancel:   cancel,
		done:     done,
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Logf("warning: server did not stop within 15s after context cancel")
		}
	})

	return ts
}

// waitForTCP dials addr in a polling loop until the connection succeeds or
// timeout expires.  Returns true if the port became reachable.
func waitForTCP(tb testing.TB, addr string, timeout time.Duration) bool {
	tb.Helper()
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := dialer.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// waitForReadyz polls GET /readyz until it returns 200 or timeout elapses.
// This ensures both SQLite and the embedded NATS server (including JetStream
// stream provisioning) are fully up before tests start.
func waitForReadyz(tb testing.TB, httpAddr string, timeout time.Duration) bool {
	tb.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + httpAddr + "/readyz"
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// ── Mock worker ───────────────────────────────────────────────────────────────

// mockWorker is a minimal in-process NATS client that behaves like a real
// sqi-worker: it registers with the server, requests task leases, and
// publishes status + log messages.  It does not execute any OS processes.
type mockWorker struct {
	t       *testing.T
	id      string
	farmID  string
	queueID string
	nc      *nats.Conn
	js      jetstream.JetStream
}

// newMockWorker dials the embedded NATS server at natsURL and returns a ready
// *mockWorker.  The connection is closed via t.Cleanup.
func newMockWorker(t *testing.T, natsURL, workerID, farmID, queueID string) *mockWorker {
	t.Helper()

	nc, err := nats.Connect(
		natsURL,
		nats.MaxReconnects(5),
		nats.ReconnectWait(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("mockWorker: nats.Connect(%q): %v", natsURL, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		t.Fatalf("mockWorker: jetstream.New: %v", err)
	}

	t.Cleanup(func() {
		if !nc.IsClosed() {
			nc.Close()
		}
	})

	return &mockWorker{
		t:       t,
		id:      workerID,
		farmID:  farmID,
		queueID: queueID,
		nc:      nc,
		js:      js,
	}
}

// register publishes a [protocol.RegisterMsg] to worker.register so the
// server records this worker as online and eligible for task assignment.
func (w *mockWorker) register() {
	w.t.Helper()

	msg := protocol.RegisterMsg{
		Version:            protocol.ProtocolVersion,
		Type:               protocol.TypeRegister,
		WorkerID:           w.id,
		FarmID:             w.farmID,
		QueueID:            w.queueID,
		Hostname:           "test-worker",
		OS:                 "linux",
		CPUCount:           4,
		RAMMb:              8192,
		MaxConcurrentTasks: 1,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		w.t.Fatalf("mockWorker.register: marshal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.js.Publish(ctx, bus.WorkerRegisterSubject(w.id), data); err != nil {
		w.t.Fatalf("mockWorker.register: publish: %v", err)
	}
}

// startHeartbeat launches a background goroutine that publishes a
// [protocol.HeartbeatMsg] every interval until the test ends.  Call this
// after register() to prevent the server's heartbeat sweep from marking the
// worker offline during long test runs.
func (w *mockWorker) startHeartbeat(interval time.Duration) {
	w.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	w.t.Cleanup(cancel)

	go func() {
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
				// Ignore publish errors in the heartbeat loop — the connection
				// may be closing during test cleanup.
				if _, pubErr := w.js.Publish(pubCtx, bus.WorkerHeartbeatSubject(w.id), data); pubErr != nil {
					pubCancel()
					return
				}
				pubCancel()
			}
		}
	}()
}

// pullAssignment requests a work lease on work.lease.<queue> and blocks until
// the server returns a non-empty batch or timeout expires.  It returns the
// first decoded [protocol.AssignMsg] in the reply.  The request timeout exceeds
// the server's long-poll hold so a parked request is never abandoned (which
// would strand a leased task).
func (w *mockWorker) pullAssignment(timeout time.Duration) protocol.AssignMsg {
	w.t.Helper()

	reqBytes, err := json.Marshal(struct {
		WorkerID string `json:"worker_id"`
	}{w.id})
	if err != nil {
		w.t.Fatalf("mockWorker.pullAssignment: marshal request: %v", err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		reqTimeout := min(time.Until(deadline), 35*time.Second)
		reqCtx, cancel := context.WithTimeout(context.Background(), reqTimeout)
		msg, reqErr := w.nc.RequestWithContext(reqCtx, bus.WorkLeaseSubject(w.id, w.queueID), reqBytes)
		cancel()
		if reqErr != nil {
			if !errors.Is(reqErr, context.DeadlineExceeded) && !errors.Is(reqErr, nats.ErrTimeout) {
				w.t.Logf("mockWorker.pullAssignment: request error: %v", reqErr)
			}
			continue
		}
		var rep struct {
			Assignments []json.RawMessage `json:"assignments"`
		}
		if err := json.Unmarshal(msg.Data, &rep); err != nil {
			w.t.Fatalf("mockWorker.pullAssignment: unmarshal reply: %v", err)
		}
		if len(rep.Assignments) == 0 {
			continue // server had no work; ask again
		}
		var assign protocol.AssignMsg
		if err := json.Unmarshal(rep.Assignments[0], &assign); err != nil {
			w.t.Fatalf("mockWorker.pullAssignment: unmarshal assignment: %v", err)
		}
		return assign
	}
	w.t.Fatalf("mockWorker.pullAssignment: no assignment received within %s", timeout)
	return protocol.AssignMsg{} // unreachable
}

// publishStatus publishes a [protocol.TaskStatusMsg] to task.status.<jobID>.
func (w *mockWorker) publishStatus(assign protocol.AssignMsg, status string, exitCode *int) {
	w.t.Helper()

	msg := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    assign.TaskID,
		AttemptID: assign.AttemptID,
		JobID:     assign.JobID,
		Status:    status,
		ExitCode:  exitCode,
		SessionID: "test-session-" + assign.TaskID,
		At:        time.Now().UTC(),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		w.t.Fatalf("mockWorker.publishStatus(%s): marshal: %v", status, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.js.Publish(ctx, bus.TaskStatusSubject(w.id, assign.JobID), data); err != nil {
		w.t.Fatalf("mockWorker.publishStatus(%s): publish: %v", status, err)
	}
}

// publishLogChunk publishes a [protocol.LogChunkMsg] to task.logs.<taskID>.
func (w *mockWorker) publishLogChunk(assign protocol.AssignMsg, seqNum int64, data string) {
	w.t.Helper()

	msg := protocol.LogChunkMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeLogChunk,
		TaskID:    assign.TaskID,
		AttemptID: assign.AttemptID,
		SeqNum:    seqNum,
		At:        time.Now().UTC(),
		Stream:    "stdout",
		Data:      data,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		w.t.Fatalf("mockWorker.publishLogChunk: marshal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.js.Publish(ctx, bus.TaskLogsSubject(w.id, assign.TaskID), raw); err != nil {
		w.t.Fatalf("mockWorker.publishLogChunk: publish: %v", err)
	}
}
