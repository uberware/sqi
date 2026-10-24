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
	"slices"
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

	// Logs holds every record this server logged at Warn or above (what the
	// harness also prints to stderr), so a test can assert on what the server
	// said. The harness boots the server without a diagnostics buffer, so
	// /api/v1/diagnostics/logs is off; this is the in-process equivalent.
	Logs *logCapture

	// cfg is the configuration the server was booted with, so
	// [restartServer] can start another on the same ports, store and NATS
	// directory. Set by [bootServer]; a test may edit it (to point the next
	// start at another database, say) between [testServer.Stop] and the
	// restart.
	cfg server.Config

	cancel context.CancelFunc
	done   chan error

	// chain is every server booted on this server's ports, store and NATS
	// directory, this one and its [restartServer] successors; the cleanup
	// registered by the first stops them all.
	chain *serverChain

	stopOnce sync.Once
	stopErr  error
}

// stopWait bounds how long [testServer.stop] waits for Run to return. Run
// itself gives its shutdown up to [server.ShutdownTimeout]; a shorter wait
// here would give up on a server that is still shutting down and still holds
// its ports, store and NATS directory, so the next start on them (a restart,
// a restore moving the NATS directory aside) would race it.
const stopWait = server.ShutdownTimeout + 5*time.Second

// Stop shuts the server down and waits for it to finish, so another can start
// on the same ports, store and NATS directory. It fails the test when the
// server does not stop within stopWait or stops with an error: a runbook that
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
		case <-time.After(stopWait):
			ts.stopErr = fmt.Errorf("server did not stop within %s after context cancel", stopWait)
		}
	})
	return ts.stopErr
}

// serverChain is the servers booted one after another on the same ports,
// store and NATS directory: a server and its [restartServer] successors.
//
// One cleanup, registered when the first boots, stops them all, so the
// servers stop after every worker the test started against any of them.
// t.Cleanup runs last in, first out: a cleanup registered per restart would
// stop that server before the workers started against its predecessor, and
// those workers would shut down against a broker that is gone — their
// deregistration never delivered, their drain failing with "nats: connection
// reconnecting", and a task's terminal status left in a reconnect buffer
// nothing flushes.
type serverChain struct {
	mu      sync.Mutex
	servers []*testServer
}

// add records ts as the chain's newest server.
func (c *serverChain) add(ts *testServer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts.chain = c
	c.servers = append(c.servers, ts)
}

// stopAll stops every server in the chain, newest first; stopping one already
// stopped is a no-op.
func (c *serverChain) stopAll(t *testing.T) {
	c.mu.Lock()
	servers := slices.Clone(c.servers)
	c.mu.Unlock()
	for _, ts := range slices.Backward(servers) {
		if err := ts.stop(); err != nil {
			t.Logf("warning: stopping server (HTTP %s, NATS %s): %v", ts.HTTPAddr, ts.NATSAddr, err)
		}
	}
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
//
// The new server joins ts's chain rather than registering a cleanup of its own,
// so it is stopped where ts would have been: after the workers the test started
// against ts (see [serverChain]).
func restartServer(t *testing.T, ts *testServer) *testServer {
	t.Helper()
	next, err := launchServer(t, ts.cfg, ts.Store)
	if err != nil {
		t.Fatalf("restartServer: HTTP %s, NATS %s: %v", ts.cfg.HTTPAddr, ts.cfg.NATSAddr, err)
	}
	if ts.chain != nil {
		ts.chain.add(next)
	} else {
		// ts came from a constructor outside this file's chain; fall back to
		// a cleanup of the new server's own.
		chain := &serverChain{}
		chain.add(next)
		t.Cleanup(func() { chain.stopAll(t) })
	}
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
	ts, err := launchServer(t, cfg, so)
	if err != nil {
		return nil, err
	}
	chain := &serverChain{}
	chain.add(ts)
	t.Cleanup(func() { chain.stopAll(t) })
	return ts, nil
}

// serverStartupTimeout bounds each wait for a booting test server (listening,
// then /readyz). Startup runs migrations that fsync, and a single stalled flush
// on a shared Windows CI runner has taken over 10s, failing the harness with no
// fault in the server; a healthy boot is far under a second, so a generous bound
// costs nothing.
const serverStartupTimeout = 60 * time.Second

// launchServer is [tryBootServer] without the cleanup: the caller registers
// one, or adds the server to a chain whose cleanup is already registered.
func launchServer(t *testing.T, cfg server.Config, so backend.Options) (*testServer, error) {
	t.Helper()

	// A server that cannot bind its HTTP address does not fail: Run logs the
	// error from its listener goroutine and carries on, and the startup below
	// only times out ("HTTP server did not come up"), hiding the cause. So
	// wait, bounded, until both addresses can be bound before booting. On a
	// restart the ports belonged to a server that just stopped, and freePort
	// draws from the same ephemeral range as outgoing connections' source
	// ports (a reconnecting worker, a database client), so the OS may lend one
	// out while the server is down. Linux makes that rare (connect prefers
	// even ports, bind(0) odd ones); other OSes draw both from one pool.
	if err := waitAddrsBindable(10*time.Second, cfg.HTTPAddr, cfg.NATSAddr); err != nil {
		return nil, err
	}

	logs := &logCapture{start: time.Now()}
	logger := slog.New(&captureHandler{
		base: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelWarn, // suppress info noise in test output
		}),
		logs: logs,
	})

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
		Logs:     logs,
		cfg:      cfg,
		cancel:   cancel,
		done:     done,
	}

	// Wait until the HTTP server is ready to accept connections, failing with
	// the server's own error if it exits first (a port in use, a store error).
	// The diagnostics are read before the server is stopped: stopping unwinds
	// the very goroutines whose stacks say what startup was blocked on.
	if err := waitForServerTCP(done, cfg.HTTPAddr, serverStartupTimeout); err != nil {
		err = fmt.Errorf("%w%s", err, startupDiagnostics(logs))
		_ = ts.stop() //nolint:errcheck // the startup error is the one to report
		return nil, err
	}

	// Poll /readyz (which gates on NATS and the store being healthy) rather than
	// sleeping a fixed duration.  This ensures JetStream streams are fully
	// provisioned before the test starts publishing.
	if !waitForReadyz(t, cfg.HTTPAddr, serverStartupTimeout) {
		err := fmt.Errorf("server did not become ready (/readyz) within %s%s", serverStartupTimeout, startupDiagnostics(logs))
		_ = ts.stop() //nolint:errcheck // not becoming ready is the error to report
		return nil, err
	}
	return ts, nil
}

// waitAddrsBindable waits until a TCP listener can be bound on every one of
// addrs, releasing each at once, or fails after timeout with the last bind
// error. Any bind error is retried, not only EADDRINUSE: Windows reports the
// condition under an errno the syscall package does not name portably.
func waitAddrsBindable(timeout time.Duration, addrs ...string) error {
	deadline := time.Now().Add(timeout)
	lc := &net.ListenConfig{}
	for _, addr := range addrs {
		for {
			ln, err := lc.Listen(context.Background(), "tcp", addr)
			if err == nil {
				if err := ln.Close(); err != nil {
					return fmt.Errorf("release %s: %w", addr, err)
				}
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("address %s not bindable after %s: %w", addr, timeout, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
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

// ── Server log capture ────────────────────────────────────────────────────────

// capturedLog is one record a test server logged: its message and its
// attributes (WithAttrs-accumulated and per-record), stringified. Groups are
// not flattened in, as in internal/log's diagnostic sink.
type capturedLog struct {
	Msg   string
	Attrs map[string]string
}

// logCapture keeps the records a test server logs. Safe for concurrent use.
type logCapture struct {
	mu    sync.Mutex
	recs  []capturedLog
	trail []string  // INFO-and-above records, timestamped; see startupdiag_test.go
	start time.Time // when the logger was created
}

// has reports whether any record so far has message msg and every attribute
// in attrs, with the same value.
func (c *logCapture) has(msg string, attrs map[string]string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.recs {
		if r.Msg != msg {
			continue
		}
		match := true
		for k, v := range attrs {
			if r.Attrs[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// captureHandler hands each record to base and also keeps it in logs, so the
// capture sees exactly what base's level lets through.
type captureHandler struct {
	base  slog.Handler
	logs  *logCapture
	attrs []slog.Attr
}

// Enabled admits INFO and above even where base is quieter: those records feed
// the startup trail a failed boot reports. Only records base also enables reach
// base and logs.recs, so what tests assert on and what the test output shows are
// unchanged.
func (h *captureHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.base.Enabled(ctx, l)
}

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	h.logs.addTrail(trailLine(time.Since(h.logs.start), r, h.attrs))
	if !h.base.Enabled(ctx, r.Level) {
		return nil
	}
	err := h.base.Handle(ctx, r)
	attrs := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.logs.mu.Lock()
	h.logs.recs = append(h.logs.recs, capturedLog{Msg: r.Message, Attrs: attrs})
	h.logs.mu.Unlock()
	return err
}

func (h *captureHandler) WithAttrs(as []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(as))
	merged = append(merged, h.attrs...)
	merged = append(merged, as...)
	return &captureHandler{base: h.base.WithAttrs(as), logs: h.logs, attrs: merged}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{base: h.base.WithGroup(name), logs: h.logs, attrs: h.attrs}
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
