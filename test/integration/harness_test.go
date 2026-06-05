// SPDX-License-Identifier: AGPL-3.0-only

// Package integration provides an end-to-end test harness for sqi-server.
//
// The harness boots a full [server.Server] (SQLite on a temp file, embedded
// NATS, scheduler, HTTP) inside the test process, then drives a mock worker
// over NATS to verify the complete job lifecycle:
//
//	submit job (REST) → assignment (NATS pull) → log streaming (NATS) →
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

	srv := server.New(cfg, logger)

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
// sqi-worker: it registers with the server, pulls task assignments, and
// publishes status + log messages.  It does not execute any OS processes.
type mockWorker struct {
	t        *testing.T
	id       string
	farmID   string
	queueID  string
	nc       *nats.Conn
	js       jetstream.JetStream
	consumer jetstream.Consumer
}

// newMockWorker dials the embedded NATS server at natsURL, ensures the
// work-assignment pull consumer exists for queueID, and returns a ready
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

	// Create the durable pull consumer for the worker's queue.  The consumer
	// name and configuration mirror [bus.Client.EnsureWorkConsumer] exactly so
	// the server and mock worker share the same durable name.
	consumer, err := js.CreateOrUpdateConsumer(context.Background(), bus.StreamWork, jetstream.ConsumerConfig{
		Durable:       "sqi-work-" + sanitize(queueID),
		FilterSubject: bus.WorkAssignSubject(queueID),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		Description:   "integration test pull consumer for queue " + queueID,
	})
	if err != nil {
		t.Fatalf("mockWorker: create pull consumer: %v", err)
	}

	return &mockWorker{
		t:        t,
		id:       workerID,
		farmID:   farmID,
		queueID:  queueID,
		nc:       nc,
		js:       js,
		consumer: consumer,
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
	if _, err := w.js.Publish(ctx, bus.SubjectWorkerRegister, data); err != nil {
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
				if _, pubErr := w.js.Publish(pubCtx, bus.SubjectWorkerHeartbeat, data); pubErr != nil {
					pubCancel()
					return
				}
				pubCancel()
			}
		}
	}()
}

// pullAssignment blocks until a task-assignment message is delivered or
// timeout expires.  It acks the message and returns the decoded [protocol.AssignMsg].
func (w *mockWorker) pullAssignment(timeout time.Duration) protocol.AssignMsg {
	w.t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := w.consumer.Fetch(1, jetstream.FetchMaxWait(500*time.Millisecond))
		if err != nil {
			// Distinguish transient fetch-window expiry from hard errors.
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, nats.ErrTimeout) {
				w.t.Logf("mockWorker.pullAssignment: Fetch error: %v", err)
			}
			continue
		}
		for msg := range msgs.Messages() {
			var assign protocol.AssignMsg
			if err := json.Unmarshal(msg.Data(), &assign); err != nil {
				if nakErr := msg.Nak(); nakErr != nil {
					w.t.Logf("mockWorker.pullAssignment: nak error: %v", nakErr)
				}
				w.t.Fatalf("mockWorker.pullAssignment: unmarshal: %v", err)
			}
			if err := msg.Ack(); err != nil {
				w.t.Logf("mockWorker.pullAssignment: ack error (non-fatal): %v", err)
			}
			return assign
		}
		// Log non-timeout batch errors but keep polling.
		if batchErr := msgs.Error(); batchErr != nil &&
			!errors.Is(batchErr, context.DeadlineExceeded) &&
			!errors.Is(batchErr, nats.ErrTimeout) {
			w.t.Logf("mockWorker.pullAssignment: batch error: %v", batchErr)
		}
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
	if _, err := w.js.Publish(ctx, bus.TaskStatusSubject(assign.JobID), data); err != nil {
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
	if _, err := w.js.Publish(ctx, bus.TaskLogsSubject(assign.TaskID), raw); err != nil {
		w.t.Fatalf("mockWorker.publishLogChunk: publish: %v", err)
	}
}

// sanitize replaces non-alphanumeric, non-dash, non-underscore, non-dot
// characters with underscores — mirrors the bus package's internal helper so
// the test uses the same consumer name the server expects.
//
// Note: this uses byte-level indexing like the original, which is correct for
// UUID-format IDs (all ASCII).  If IDs ever contain multibyte characters this
// should switch to strings.Map with rune semantics, matching the bus package.
func sanitize(s string) string {
	out := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			out[i] = c
		} else {
			out[i] = '_'
		}
	}
	return string(out)
}
