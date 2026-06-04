// SPDX-License-Identifier: AGPL-3.0-only

// Package server owns the sqi-server component lifecycle: starting and
// stopping the store, message bus, scheduler, HTTP server, and mDNS
// responder in the correct dependency order.
//
// Components are added to [Server] as their implementing tasks land:
//   - store (SQLite):          tasks 25–32 ✓
//   - bus (NATS JetStream):    tasks 33–39 ✓
//   - scheduler:               tasks 46–55 ✓ (46–48 done)
//   - httpServer (chi router): task 70 ✓ — REST+WS+UI routes added tasks 71–95
//   - discovery (mDNS):        tasks 96–97
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/ws"
)

// ShutdownTimeout is the maximum time [Server.Run] waits for all components
// to stop cleanly before returning. Components that do not respect context
// cancellation within this window are abandoned.
const ShutdownTimeout = 30 * time.Second

// Config holds runtime parameters for the server.
//
// This struct is a temporary stand-in used by the serve subcommand until
// internal/config is introduced in tasks 16–19. At that point, the serve
// subcommand will load a [config.Config] and derive a [Config] from it rather
// than using defaults.
type Config struct {
	// HTTPAddr is the TCP address the REST + WebSocket server listens on.
	HTTPAddr string // default "0.0.0.0:8080"

	// CORSOrigins is the list of origins that the CORS middleware permits.
	// Use ["*"] to allow all origins (suitable for local / dev deployments).
	// An empty slice is treated as ["*"]. Tighten this for production
	// deployments once the web UI's origin is known.
	CORSOrigins []string // default ["*"]

	// EnablePprof registers the Go runtime profiling endpoints at
	// /debug/pprof/ when true. Should never be enabled on servers accessible
	// to untrusted networks. Default false.
	EnablePprof bool

	// NATSAddr is the TCP address the embedded NATS server listens on.
	// It defaults to loopback so external clients cannot reach it directly;
	// the sqi-server communicates with NATS in-process.
	NATSAddr string // default "127.0.0.1:4222"

	// NATSDataDir is the directory used by JetStream for file-backed stream
	// storage. It is created at startup if it does not exist.
	NATSDataDir string // default "data/nats"

	// NATSMaxStoreMB is the maximum disk space JetStream may use, in
	// megabytes.  0 means unlimited (not recommended for production).
	NATSMaxStoreMB int // default 1024

	// SQLitePath is the path to the SQLite database file used in simple mode.
	// It is created at startup if it does not exist.
	SQLitePath string // default "sqi.db"

	// CheckpointInterval is how often the background WAL checkpointer runs.
	// See internal/config.StoreConfig.CheckpointInterval for semantics.
	CheckpointInterval time.Duration // default 5m

	// Scheduler holds tuning parameters for the assignment loop, worker
	// registry, and heartbeat sweep. Zero values use scheduler.DefaultConfig().
	Scheduler scheduler.Config
}

// DefaultConfig returns a [Config] with sensible development defaults.
// Production deployments override these via the config file or environment
// variables (tasks 16–19).
func DefaultConfig() Config {
	return Config{
		HTTPAddr:           "0.0.0.0:8080",
		NATSAddr:           "127.0.0.1:4222",
		NATSDataDir:        "data/nats",
		NATSMaxStoreMB:     1024,
		SQLitePath:         "sqi.db",
		CheckpointInterval: 5 * time.Minute,
		Scheduler:          scheduler.DefaultConfig(),
	}
}

// Server coordinates the startup and shutdown of all sqi-server components.
// It is created by [New] and driven by [Run].
type Server struct {
	cfg     Config
	logger  *slog.Logger
	metrics *metrics.Metrics
	health  *health.Registry

	// httpServer is the chi-based HTTP server that handles the full REST API,
	// WebSocket connections, embedded web UI, and observability routes
	// (/metrics, /healthz, /readyz, /debug/pprof). Introduced in task 70.
	httpServer *http.Server

	// Component fields are added here as their tasks land.
	store     store.Store          // tasks 25–32 ✓
	broker    *bus.Broker          // tasks 33–39 ✓
	busClient *bus.Client          // tasks 36–39 ✓ — typed wrapper; drained before broker shutdown
	sched     *scheduler.Scheduler // tasks 46–59 ✓
	wsHub     *ws.Hub              // tasks 89–91 ✓ — WebSocket fan-out hub
	// discovery *discovery.Responder   // tasks 96–97
}

// New creates a [Server] with the given configuration and logger.
func New(cfg Config, logger *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: metrics.New(),
		health:  health.NewRegistry(),
	}
}

// Metrics returns the [*metrics.Metrics] instance owned by this server.
// Other components (scheduler, bus, store) use it to record observations.
func (s *Server) Metrics() *metrics.Metrics {
	return s.metrics
}

// Health returns the [*health.Registry] owned by this server. Components
// (store, bus) call Health().Register(...) during startup to participate in
// readiness gating via GET /readyz.
func (s *Server) Health() *health.Registry {
	return s.health
}

// Run starts all server components and blocks until ctx is canceled (typically
// by a SIGINT or SIGTERM delivered via [signal.NotifyContext]). It then
// initiates a graceful shutdown and returns once all components have stopped or
// [ShutdownTimeout] is exceeded.
//
// A nil return value means the server started and shut down without errors.
// Startup failures return immediately with a non-nil error.
func (s *Server) Run(ctx context.Context) error {
	s.logger.InfoContext(
		ctx, "sqi-server starting",
		slog.String("http_addr", s.cfg.HTTPAddr),
		slog.String("nats_addr", s.cfg.NATSAddr),
		slog.String("sqlite_path", s.cfg.SQLitePath),
	)

	if err := s.start(ctx); err != nil {
		return fmt.Errorf("startup failed: %w", err)
	}

	s.logger.InfoContext(ctx, "sqi-server ready — waiting for signal")

	// Block until the calling context is canceled.
	<-ctx.Done()

	s.logger.InfoContext(ctx, "shutdown signal received, stopping components")
	return s.shutdown()
}

// start brings up all components in dependency order.
// Each block is a stub that will be replaced as the corresponding tasks land.
func (s *Server) start(ctx context.Context) error {
	// ── Store (SQLite) ─────────────────────────────────────────────────────
	st, err := sqlite.Open(ctx, s.cfg.SQLitePath, sqlite.DefaultOptions())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	s.store = st
	s.logger.InfoContext(ctx, "store: sqlite open", slog.String("path", s.cfg.SQLitePath))

	// Register SQLite as a readiness dependency so /readyz reflects its health.
	s.health.Register("sqlite", health.CheckerFunc(st.Ping))

	// Start the background WAL checkpointer. It exits when ctx is canceled,
	// running one final checkpoint before returning so the WAL is fully flushed
	// before store.Close is called in shutdown.
	st.StartCheckpointer(ctx, s.cfg.CheckpointInterval, s.logger)
	s.logger.InfoContext(
		ctx, "store: wal checkpointer started",
		slog.Duration("interval", s.cfg.CheckpointInterval),
	)

	// ── Message bus (NATS JetStream) ───────────────────────────────────────
	// Tasks 33–35: embed NATS server, enable JetStream, provision streams.
	// Tasks 36–39: typed client wrapper, consumers, reconnect, drain.
	broker := bus.New(bus.BrokerConfig{
		Addr:       s.cfg.NATSAddr,
		DataDir:    s.cfg.NATSDataDir,
		MaxStoreMB: s.cfg.NATSMaxStoreMB,
	}, s.logger)
	if err := broker.Start(ctx); err != nil {
		return fmt.Errorf("start bus: %w", err)
	}
	s.broker = broker

	// Register NATS as a readiness dependency so GET /readyz reflects its health.
	s.health.Register("nats", health.CheckerFunc(broker.Check))

	// Dial the embedded broker with a typed client.  All server components
	// (scheduler, worker-protocol handlers, etc.) will receive this client
	// rather than importing the raw nats package directly.
	busClient, err := broker.NewClient()
	if err != nil {
		return fmt.Errorf("start bus client: %w", err)
	}
	s.busClient = busClient
	s.logger.InfoContext(ctx, "bus: typed client connected")

	// ── WebSocket hub (tasks 89–91) ────────────────────────────────────────
	// The hub bridges scheduler events to subscribed WebSocket clients.
	// It is created before the scheduler so it can be passed as the notifier.
	s.wsHub = ws.NewHub(s.logger)
	s.logger.InfoContext(ctx, "ws: hub created")

	// ── Scheduler ─────────────────────────────────────────────────────────
	// Tasks 46–48: assignment loop goroutine pool, worker registry (NATS
	// consumer), and heartbeat timeout sweep.
	// The hub is passed as the notifier (tasks 89–91) so live events are
	// pushed to subscribed WebSocket clients after each state change.
	s.sched = scheduler.New(s.cfg.Scheduler, s.store, s.busClient, s.metrics, s.logger, s.wsHub)
	go func() {
		if err := s.sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.ErrorContext(ctx, "scheduler: exited with error", slog.Any("error", err))
		}
	}()
	s.logger.InfoContext(ctx, "scheduler: started")

	// ── HTTP server (chi router) ──────────────────────────────────────────
	// Task 70: chi router with standard middleware mounts all routes.
	// Task 71–75: job REST endpoints are now registered via api.Deps.
	router := api.NewRouter(
		api.Config{
			CORSOrigins: s.cfg.CORSOrigins,
			EnablePprof: s.cfg.EnablePprof,
		},
		api.Deps{
			Store:     s.store,
			Submitter: openjd.NewSubmitter(s.store),
			Scheduler: s.sched,
			Hub:       s.wsHub,
		},
		s.logger,
		s.metrics,
		s.health,
	)

	s.httpServer = &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		s.logger.InfoContext(ctx, "http: listening", slog.String("addr", s.cfg.HTTPAddr))
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.ErrorContext(ctx, "http: server error", slog.Any("error", err))
		}
	}()

	// ── mDNS responder ────────────────────────────────────────────────────
	// TODO(tasks 96–97): advertise _sqi._tcp on the local network.
	s.logger.DebugContext(ctx, "discovery: not yet started (tasks 96–97)")

	return nil
}

// shutdown stops all running components in reverse dependency order within
// a [ShutdownTimeout] deadline.
func (s *Server) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()

	var errs []error

	// Stop in reverse startup order.

	// ── mDNS responder ────────────────────────────────────────────────────
	// TODO(tasks 96–97): unregister mDNS service.

	// ── HTTP server ───────────────────────────────────────────────────────
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("http shutdown: %w", err))
		}
	}

	// ── Scheduler ─────────────────────────────────────────────────────────
	// Signal the scheduler to stop. Its goroutines exit when their context is
	// canceled; the bus client drain below ensures any in-flight NATS messages
	// are processed before the connection closes.
	if s.sched != nil {
		s.sched.Stop()
	}

	// ── Message bus ───────────────────────────────────────────────────────
	// Tasks 36–39: drain the typed client first — stops push consumers,
	// waits for in-flight handlers, and flushes pending publish-acks — then
	// shut down the embedded NATS server.
	if s.busClient != nil {
		if err := s.busClient.Drain(ctx); err != nil {
			errs = append(errs, fmt.Errorf("bus client drain: %w", err))
		}
	}
	if s.broker != nil {
		s.broker.Shutdown()
	}

	// ── Store ─────────────────────────────────────────────────────────────
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store close: %w", err))
		}
	}

	if ctx.Err() != nil {
		errs = append(errs, fmt.Errorf("graceful shutdown timed out after %s", ShutdownTimeout))
	}

	if len(errs) == 0 {
		s.logger.InfoContext(ctx, "sqi-server stopped cleanly")
		return nil
	}

	return errors.Join(errs...)
}
