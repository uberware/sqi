// SPDX-License-Identifier: AGPL-3.0-or-later

// Package integration provides an end-to-end test harness for sqi-server.
//
// The harness boots a full [server.Server] (SQLite on a temp file by default,
// or a fresh PostgreSQL database when SQI_TEST_STORE_DRIVER=postgres; embedded
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
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Store selection ───────────────────────────────────────────────────────────

// storeFor returns the store a test server uses: SQLite at sqlitePath by
// default, or, when SQI_TEST_STORE_DRIVER=postgres, a fresh PostgreSQL
// database (make test-postgres sets both). It takes a [testing.TB] because
// the load harness also serves benchmarks.
//
// A value other than "", "sqlite" or "postgres" fails the test: a typo must
// not run SQLite under a PostgreSQL label.
func storeFor(tb testing.TB, sqlitePath string) backend.Options {
	tb.Helper()
	switch d := os.Getenv("SQI_TEST_STORE_DRIVER"); d {
	case backend.DriverPostgres:
		return backend.Options{Driver: backend.DriverPostgres, PostgresDSN: storetest.PostgresDSN(tb), MaxConns: 8}
	case "", backend.DriverSQLite:
		return backend.Options{Driver: backend.DriverSQLite, SQLitePath: sqlitePath}
	default:
		tb.Fatalf("SQI_TEST_STORE_DRIVER=%q: want %q or %q", d, backend.DriverSQLite, backend.DriverPostgres)
		return backend.Options{} // unreachable
	}
}

// applyStore points a server config at o.
func applyStore(cfg *server.Config, o backend.Options) {
	cfg.StoreDriver = backend.Name(o.Driver)
	cfg.SQLitePath = o.SQLitePath
	cfg.PostgresDSN = o.PostgresDSN
	if o.MaxConns > 0 {
		cfg.PostgresMaxConns = o.MaxConns
	}
}

// openTestStore opens a second handle on a test server's store (migrating a
// SQLite file that does not exist yet), closed when the test ends.
func openTestStore(tb testing.TB, o backend.Options) store.Store {
	tb.Helper()
	o.AutoMigrate = true
	st, err := backend.Open(context.Background(), o)
	if err != nil {
		tb.Fatalf("open test store: %v", err)
	}
	tb.Cleanup(func() { _ = st.Close() })
	return st
}

// ── Port helpers ──────────────────────────────────────────────────────────────

var (
	handedOutMu    sync.Mutex
	handedOutPorts = map[int]struct{}{}
)

// freePort asks the OS for an available TCP port on loopback and returns it.
// The port is released immediately after being discovered, so there is a
// tiny window where another process could claim it.  In practice this is
// acceptable for integration tests on CI where ports are not heavily contested.
//
// It never returns the same port twice within a process. The kernel is free to
// hand a just-released port out again, and two of a server's listeners (HTTP
// and NATS) sharing one port makes the second bind fail while a TCP probe of
// the first still succeeds, which reads as a server that never became ready.
func freePort(tb testing.TB) int {
	tb.Helper()
	const maxDraws = 100
	for range maxDraws {
		port := drawPort(tb)
		handedOutMu.Lock()
		_, seen := handedOutPorts[port]
		if !seen {
			handedOutPorts[port] = struct{}{}
		}
		handedOutMu.Unlock()
		if !seen {
			return port
		}
	}
	tb.Fatalf("freePort: %d draws all returned a port already handed out", maxDraws)
	return 0 // unreachable
}

// drawPort binds loopback port 0, reads the port the OS chose, and releases it.
func drawPort(tb testing.TB) int {
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
	// Scheme is "http" or "https", matching whether this server terminates
	// TLS. Empty means "http". apiURL and every helper built on it read this,
	// so a TLS server needs no parallel set of helpers.
	Scheme string

	// Client performs requests against this server. Nil means the package's
	// default plaintext client; a TLS server sets one trusting its farm CA.
	Client *http.Client

	// Store is the store this server was started against: SQLite at a temp
	// path by default, a fresh PostgreSQL database under
	// SQI_TEST_STORE_DRIVER=postgres. A caller that needs to inspect store
	// state the REST API does not expose (e.g. confirming no row was ever
	// written to a table) passes it to openTestStore for a second, independent
	// handle — safe on SQLite because the store runs in WAL mode, which allows
	// a reader to run alongside the server's own writer, and on PostgreSQL
	// because a second handle takes no single-server lock.
	Store backend.Options

	// DBPath is the SQLite file this server was started against. SQLite only:
	// empty on PostgreSQL. Set by constructors that know it, empty otherwise.
	// Prefer Store, which names the right database on either driver.
	DBPath string

	// NATSDir is the embedded NATS server's JetStream data directory. Set by
	// [bootServer]; a test that restores a backup moves it aside or swaps it.
	NATSDir string

	// cfg is the configuration the server was booted with, so
	// [restartServer] can start another on the same ports, store and NATS
	// directory. Set by [bootServer]; a test may edit it (to point the next
	// start at another database, say) between [testServer.Stop] and the
	// restart.
	cfg server.Config

	cancel context.CancelFunc
	done   chan error

	stopOnce sync.Once
	stopErr  error
}

// Stop shuts the server down and waits for it to finish, so another can start
// on the same ports, store and NATS directory. It fails the test when the
// server does not stop within 15 s or stops with an error: a runbook that
// stops the server needs a clean stop. Safe to call more than once, and with
// the cleanup that also stops it.
func (ts *testServer) Stop(t *testing.T) {
	t.Helper()
	if err := ts.stop(); err != nil {
		t.Fatalf("stop server (HTTP %s, NATS %s): %v", ts.HTTPAddr, ts.NATSAddr, err)
	}
}

// stop cancels the server and waits for Run to return, once; later calls
// return the first call's result.
func (ts *testServer) stop() error {
	ts.stopOnce.Do(func() {
		ts.cancel()
		select {
		case ts.stopErr = <-ts.done:
		case <-time.After(15 * time.Second):
			ts.stopErr = errors.New("server did not stop within 15s after context cancel")
		}
	})
	return ts.stopErr
}

// startServer boots a full sqi-server on the store storeFor picks (a temp
// SQLite file, or a fresh PostgreSQL database under
// SQI_TEST_STORE_DRIVER=postgres), a temp NATS data directory, and scheduler
// tuned for fast test cycles.  It blocks until the
// HTTP server is accepting connections and /readyz returns 200 (verifying NATS
// is up and streams are provisioned), then returns a *testServer.
//
// The server is stopped automatically via t.Cleanup.
func startServer(t *testing.T) *testServer {
	t.Helper()
	return startServerWith(t, nil)
}

// startServerWith is [startServer] with mutate, when non-nil, applied to the
// configuration before the server boots. A mutate that points the server at
// another store (with [applyStore]) is honored: the returned server's Store
// describes the store it actually opened.
func startServerWith(t *testing.T, mutate func(*server.Config)) *testServer {
	t.Helper()

	httpPort := freePort(t)
	natsPort := freePort(t)

	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	natsAddr := fmt.Sprintf("127.0.0.1:%d", natsPort)

	// Temp directories for the SQLite file (when that is the store) and NATS
	// JetStream storage.
	tmpDir := t.TempDir()
	so := storeFor(t, tmpDir+"/sqi-test.db")
	natsDir := tmpDir + "/nats"

	cfg := server.Config{
		HTTPAddr:           httpAddr,
		CORSOrigins:        []string{"*"},
		EnablePprof:        false,
		NATSAddr:           natsAddr,
		NATSDataDir:        natsDir,
		NATSMaxStoreMB:     64,
		CheckpointInterval: time.Minute,
		// Tight scheduler timing so tests don't wait a full second per poll.
		Scheduler: scheduler.Config{
			AssignInterval:         100 * time.Millisecond,
			AssignBatchSize:        10,
			AssignWorkers:          2,
			WorkerTimeout:          30 * time.Second,
			HeartbeatSweepInterval: 15 * time.Second,
		},
		// mDNS disabled: these tests reach the server by an explicit address, so
		// advertising would add a real network side effect for no coverage.
		//
		// Not because multicast is unavailable: discovery_test.go proves an
		// mDNS round trip on a real interface, and `make test-discovery` fails
		// rather than skips when one is not possible.
		DiscoveryEnabled: false,
	}
	applyStore(&cfg, so)
	if mutate != nil {
		mutate(&cfg)
		so = storeOf(cfg, so)
	}
	return bootServer(t, cfg, so)
}

// storeOf returns the store cfg names, keeping prev's PostgreSQL pool size
// when cfg sets none.
func storeOf(cfg server.Config, prev backend.Options) backend.Options {
	o := backend.Options{
		Driver:      backend.Name(cfg.StoreDriver),
		SQLitePath:  cfg.SQLitePath,
		PostgresDSN: cfg.PostgresDSN,
		MaxConns:    cfg.PostgresMaxConns,
	}
	if o.MaxConns == 0 {
		o.MaxConns = prev.MaxConns
	}
	return o
}

// restartServer boots a new server with ts's configuration: the same ports,
// store and NATS directory. Stop ts first. Edit ts.cfg and ts.Store together
// beforehand to start it on another database.
func restartServer(t *testing.T, ts *testServer) *testServer {
	t.Helper()
	next := bootServer(t, ts.cfg, ts.Store)
	next.Scheme, next.Client = ts.Scheme, ts.Client
	return next
}

// bootServer runs a server with cfg, on store so (which must be the store cfg
// names), and waits until it is ready; see [startServer]. The server is
// stopped automatically via t.Cleanup.
func bootServer(t *testing.T, cfg server.Config, so backend.Options) *testServer {
	t.Helper()
	ts, err := tryBootServer(t, cfg, so)
	if err != nil {
		t.Fatalf("startServer: HTTP %s, NATS %s: %v", cfg.HTTPAddr, cfg.NATSAddr, err)
	}
	return ts
}

// tryBootServer is [bootServer] returning, instead of failing the test on, a
// server that exits during startup (a port in use, a store error) or never
// becomes ready; that server has stopped by the time it returns. The error
// wraps the server's own, so errors.Is sees through it.
func tryBootServer(t *testing.T, cfg server.Config, so backend.Options) (*testServer, error) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn, // suppress info noise in test output
	}))

	srv := server.New(cfg, logger, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
	}()

	ts := &testServer{
		HTTPAddr: cfg.HTTPAddr,
		NATSAddr: cfg.NATSAddr,
		Store:    so,
		DBPath:   so.SQLitePath,
		NATSDir:  cfg.NATSDataDir,
		cfg:      cfg,
		cancel:   cancel,
		done:     done,
	}

	// Wait until the HTTP server is ready to accept connections, failing with
	// the server's own error if it exits first (a port in use, a store error).
	if err := waitForServerTCP(done, cfg.HTTPAddr, 10*time.Second); err != nil {
		_ = ts.stop() //nolint:errcheck // the startup error is the one to report
		return nil, err
	}

	// Poll /readyz (which gates on NATS and the store being healthy) rather than
	// sleeping a fixed duration.  This ensures JetStream streams are fully
	// provisioned before the test starts publishing.
	if !waitForReadyz(t, cfg.HTTPAddr, 10*time.Second) {
		_ = ts.stop() //nolint:errcheck // not becoming ready is the error to report
		return nil, errors.New("server did not become ready (/readyz) within 10s")
	}

	t.Cleanup(func() {
		if err := ts.stop(); err != nil {
			t.Logf("warning: stopping server: %v", err)
		}
	})

	return ts, nil
}

// clientFor returns the HTTP client to use against ts: its own when it has one
// (a TLS server trusting its farm CA), the package default otherwise.
func clientFor(ts *testServer) *http.Client {
	if ts != nil && ts.Client != nil {
		return ts.Client
	}
	return httpClient
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

// waitForServerTCP polls addr until it accepts a TCP connection. It returns the
// server's error if the server, whose Run result arrives on done, exits first,
// and an error naming the timeout if neither happens in time. A value read from
// done is put back, so the caller's cleanup still sees the server stop.
func waitForServerTCP(done chan error, addr string, timeout time.Duration) error {
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			done <- err
			return fmt.Errorf("server exited during startup: %w", err)
		default:
		}
		conn, err := dialer.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("HTTP server did not come up within %s", timeout)
}

// waitForReadyz polls GET /readyz until it returns 200 or timeout elapses.
// This ensures both the store and the embedded NATS server (including JetStream
// stream provisioning) are fully up before tests start.
func waitForReadyz(tb testing.TB, httpAddr string, timeout time.Duration) bool {
	tb.Helper()
	return waitForReadyzClient(tb, &http.Client{Timeout: 2 * time.Second}, "http://"+httpAddr+"/readyz", timeout)
}

// waitForReadyzClient is waitForReadyz against a caller-supplied client and
// base URL. A TLS-terminated server needs both: an https:// scheme, and a
// client that trusts the farm CA. See tls_test.go.
func waitForReadyzClient(tb testing.TB, client *http.Client, url string, timeout time.Duration) bool {
	tb.Helper()
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

// register publishes a [protocol.RegisterMsg] to worker.register.<workerID> so the
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

// pullAssignment requests a work lease on work.lease.<workerID>.<queue> and blocks until
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

// publishStatus publishes a [protocol.TaskStatusMsg] to task.status.<workerID>.<jobID>.
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

// publishLogChunk publishes a [protocol.LogChunkMsg] to task.logs.<workerID>.<taskID>.
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
