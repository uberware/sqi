// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testserver starts a full in-process sqi-server (SQLite, embedded NATS,
// scheduler and HTTP) for the load harness's own tests. It is importable only
// from under internal/loadtest.
package testserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/auth/jointoken"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
)

const (
	startupTimeout   = 15 * time.Second
	shutdownTimeout  = 60 * time.Second
	maxStartAttempts = 5
)

// nextPort supplies the ports a server binds. It is a variable so a test can
// hand out a port that something else already holds.
var nextPort = freePort

// errStartupFailed marks a server that stopped before it was ready: what a
// lost race for a port looks like, since sqi-server fails to start when its
// HTTP or NATS port is taken.
var errStartupFailed = errors.New("server failed to start")

// serverReadyMsg is the record server.Run logs once startup has finished,
// both listeners bound. Until then a 200 from /readyz on the drawn HTTP port
// may come from another process's server that took the port first;
// TestStart_IgnoresAnotherServersReadiness fails if the message changes.
const serverReadyMsg = "sqi-server ready — waiting for signal"

// readyWatch is a slog.Handler that closes ready on the server's ready record
// and drops every record. Once ready is closed it enables nothing, so the
// records a running server logs are not built only to be dropped.
type readyWatch struct {
	once  sync.Once
	ready chan struct{}
	seen  atomic.Bool
}

func (h *readyWatch) Enabled(_ context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo && !h.seen.Load()
}

func (h *readyWatch) Handle(_ context.Context, r slog.Record) error {
	if r.Message == serverReadyMsg {
		h.once.Do(func() {
			h.seen.Store(true)
			close(h.ready)
		})
	}
	return nil
}

func (h *readyWatch) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *readyWatch) WithGroup(string) slog.Handler { return h }

// Options configures Start.
type Options struct {
	// BrokerAuth turns NATS broker authentication on, with the enrollment
	// endpoint mounted and a reusable join token (join_token_single_use
	// false) minted before the server boots. Server.JoinToken returns it.
	BrokerAuth bool
	// Pprof mounts the /debug/pprof endpoints.
	Pprof bool
	// WorkerTimeout, HeartbeatSweepInterval and BusReconnectGrace, when set,
	// replace the scheduler's 60 s heartbeat timeout, 30 s sweep interval and
	// the reconnect grace the server derives from the timeout (which stands
	// the liveness sweep down for its first minute or so), so a test can have
	// a live worker marked offline quickly.
	WorkerTimeout          time.Duration
	HeartbeatSweepInterval time.Duration
	BusReconnectGrace      time.Duration
}

// Server is a running in-process sqi-server.
type Server struct {
	httpAddr  string
	natsAddr  string
	joinToken string
}

// HTTPURL returns the base URL of the REST and WebSocket API.
func (s *Server) HTTPURL() string { return "http://" + s.httpAddr }

// NATSURL returns the URL of the embedded NATS broker.
func (s *Server) NATSURL() string { return "nats://" + s.natsAddr }

// JoinToken returns the reusable join token of a server started with
// Options.BrokerAuth, and "" otherwise.
func (s *Server) JoinToken() string { return s.joinToken }

// Start boots a server tuned for throughput (tight assign interval, rate
// limiting off) and returns once /readyz passes. It stops the server, and waits
// for Run to return, through tb.Cleanup.
//
// A port is drawn and released before the server binds it, so a parallel test
// process can take it in between; Start retries on fresh ports when the server
// exits during startup.
func Start(tb testing.TB, opts Options) *Server {
	tb.Helper()
	var err error
	for attempt := 1; attempt <= maxStartAttempts; attempt++ {
		var srv *Server
		srv, err = startOnce(tb, opts)
		if err == nil {
			return srv
		}
		if !errors.Is(err, errStartupFailed) {
			break
		}
		tb.Logf("testserver: start attempt %d/%d: %v", attempt, maxStartAttempts, err)
	}
	tb.Fatalf("testserver: %v", err)
	return nil
}

func startOnce(tb testing.TB, opts Options) (*Server, error) {
	tb.Helper()

	// TempDir is registered before the stop cleanup below, so cleanups (LIFO)
	// stop the server before the directory is removed.
	tmpDir := tb.TempDir()

	httpAddr := fmt.Sprintf("127.0.0.1:%d", nextPort(tb))
	natsAddr := fmt.Sprintf("127.0.0.1:%d", nextPort(tb))

	cfg := server.Config{
		HTTPAddr:    httpAddr,
		CORSOrigins: []string{"*"},
		StoreDriver: backend.DriverSQLite,
		SQLitePath:  tmpDir + "/test.db",
		NATS: config.NATSConfig{Embedded: config.NATSEmbeddedConfig{
			Addr:       natsAddr,
			DataDir:    tmpDir + "/nats",
			MaxStoreMB: 128,
		}},
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
		EnablePprof:      opts.Pprof,
	}
	if opts.WorkerTimeout > 0 {
		cfg.Scheduler.WorkerTimeout = opts.WorkerTimeout
	}
	if opts.HeartbeatSweepInterval > 0 {
		cfg.Scheduler.HeartbeatSweepInterval = opts.HeartbeatSweepInterval
	}
	if opts.BusReconnectGrace > 0 {
		cfg.Scheduler.BusReconnectGrace = opts.BusReconnectGrace
	}
	var joinToken string
	if opts.BrokerAuth {
		joinToken = enableBrokerAuth(tb, &cfg)
	}

	watch := &readyWatch{ready: make(chan struct{})}
	srv := server.New(cfg, slog.New(watch), nil)
	ctx, cancel := context.WithCancel(context.Background())
	// done closes when Run returns, with its error in runErr, so both the
	// readiness wait and stop can observe the exit.
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = srv.Run(ctx)
		close(done)
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(shutdownTimeout):
				tb.Logf("testserver: server did not stop within %s", shutdownTimeout)
			}
		})
	}
	tb.Cleanup(stop)

	exited := func() error { return runErr } // read only after done closes
	if err := awaitReady(httpAddr, done, exited, watch.ready, startupTimeout); err != nil {
		stop()
		return nil, fmt.Errorf("%w (HTTP %s, NATS %s)", err, httpAddr, natsAddr)
	}
	return &Server{httpAddr: httpAddr, natsAddr: natsAddr, joinToken: joinToken}, nil
}

// enableBrokerAuth configures cfg for broker authentication and returns a join
// token minted straight into cfg's database, as the integration suite seeds
// one: the store handle used for it is closed before the server opens its own,
// because SQLite allows one writer.
func enableBrokerAuth(tb testing.TB, cfg *server.Config) string {
	tb.Helper()
	// A hand-built Config skips config defaults, and the auth callout needs a
	// positive user JWT lifetime.
	cfg.NATS.Auth = config.NATSAuthConfig{
		Enabled:                   true,
		UserTTL:                   time.Hour,
		EnrollmentEndpointEnabled: true,
		JoinTokenSingleUse:        false,
	}
	raw, hash, prefix, err := jointoken.Generate()
	if err != nil {
		tb.Fatalf("testserver: jointoken.Generate: %v", err)
	}
	ctx := context.Background()
	st, err := backend.Open(ctx, backend.Options{Driver: backend.DriverSQLite, SQLitePath: cfg.SQLitePath, AutoMigrate: true})
	if err != nil {
		tb.Fatalf("testserver: open store to seed a join token: %v", err)
	}
	now := time.Now().UTC()
	_, err = st.CreateWorkerJoinToken(ctx, store.WorkerJoinToken{
		ID: "loadtest-token", TokenHash: hash, Prefix: prefix, Name: "loadtest",
		ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
	})
	if cerr := st.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		tb.Fatalf("testserver: seed join token: %v", err)
	}
	return raw
}

var (
	handedOutMu    sync.Mutex
	handedOutPorts = map[int]struct{}{}
)

// freePort returns a loopback port the OS reports free, never the same one
// twice in a process: two listeners of one server sharing a port makes the
// second bind fail while a TCP probe of the first still succeeds.
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
	tb.Fatalf("testserver: %d port draws all returned a port already handed out", maxDraws)
	return 0
}

func drawPort(tb testing.TB) int {
	tb.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("testserver: free port: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if err := ln.Close(); err != nil {
		tb.Fatalf("testserver: close listener: %v", err)
	}
	if !ok {
		tb.Fatalf("testserver: unexpected address type %T", ln.Addr())
	}
	return tcpAddr.Port
}

// awaitReady waits for the server's ready record, then polls /readyz until it
// answers 200. It returns early if the server's Run returns, so a lost port
// is reported as such instead of as a timeout. /readyz is not polled before
// the ready record: the drawn HTTP port may be another server's until this
// one has bound it, and that server answers 200.
func awaitReady(httpAddr string, done <-chan struct{}, runErr func() error, ready <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return fmt.Errorf("%w: Run returned: %w", errStartupFailed, runErr())
	case <-ready:
	case <-timer.C:
		return fmt.Errorf("server not ready within %s", timeout)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return fmt.Errorf("%w: Run returned: %w", errStartupFailed, runErr())
		default:
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+httpAddr+"/readyz", http.NoBody)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("/readyz not ready within %s", timeout)
}
