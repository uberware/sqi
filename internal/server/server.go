// SPDX-License-Identifier: AGPL-3.0-only

// Package server owns the sqi-server component lifecycle: starting and
// stopping the store, message bus, scheduler, HTTP server, and mDNS
// responder in the correct dependency order.
//
// Components are added to [Server] as their implementing tasks land:
//   - store (SQLite):          tasks 25–32 ✓
//   - bus (NATS JetStream):    tasks 33–39 ✓ (tasks 33–35 done)
//   - scheduler:               tasks 46–55
//   - httpServer (REST+WS+UI): tasks 66–88
//   - discovery (mDNS):        tasks 89–90
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	httppprof "net/http/pprof"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/middleware"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
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
	}
}

// Server coordinates the startup and shutdown of all sqi-server components.
// It is created by [New] and driven by [Run].
type Server struct {
	cfg     Config
	logger  *slog.Logger
	metrics *metrics.Metrics
	health  *health.Registry

	// obsServer is a minimal net/http.Server that serves /metrics (task 22),
	// /healthz and /readyz (task 23), and pprof endpoints (task 24) on
	// cfg.HTTPAddr until the full chi router is introduced in task 66.
	//
	// TODO(task 66): replace obsServer with the chi-based REST + WebSocket
	// server; the /metrics, /healthz, /readyz, and /debug/pprof routes will
	// be registered on that router instead.
	obsServer *http.Server

	// Component fields are added here as their tasks land.
	store  store.Store // tasks 25–32 ✓
	broker *bus.Broker // tasks 33–39 ✓ (33–35 done; 36–39 pending)
	// scheduler *scheduler.Scheduler   // tasks 46–55
	// discovery *discovery.Responder   // tasks 89–90
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
	// Tasks 36–39 (pending): typed client wrapper, consumers, reconnect, drain.
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

	// ── Scheduler ─────────────────────────────────────────────────────────
	// TODO(tasks 46–55): start assignment loop, heartbeat sweep.
	s.logger.DebugContext(ctx, "scheduler: not yet started (tasks 46–55)")

	// ── Observability HTTP server ─────────────────────────────────────────
	// Serves /metrics (task 22), /healthz + /readyz (task 23), and pprof
	// (task 24). Replaced by the full chi router in task 66.
	mux := http.NewServeMux()
	mux.Handle("/metrics", s.metrics.Handler())
	mux.Handle("/healthz", s.health.LivenessHandler())
	mux.Handle("/readyz", s.health.ReadinessHandler())

	if s.cfg.EnablePprof {
		s.logger.WarnContext(
			ctx, "pprof: profiling endpoints enabled — do not expose to untrusted networks",
			slog.String("prefix", "/debug/pprof/"),
		)
		mux.HandleFunc("/debug/pprof/", httppprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
		mux.Handle("/debug/pprof/goroutine", httppprof.Handler("goroutine"))
		mux.Handle("/debug/pprof/heap", httppprof.Handler("heap"))
		mux.Handle("/debug/pprof/allocs", httppprof.Handler("allocs"))
		mux.Handle("/debug/pprof/block", httppprof.Handler("block"))
		mux.Handle("/debug/pprof/mutex", httppprof.Handler("mutex"))
		mux.Handle("/debug/pprof/threadcreate", httppprof.Handler("threadcreate"))
	}

	// Apply logging and metrics middleware so requests to /metrics are
	// themselves tracked and logged.
	var handler http.Handler = mux
	handler = middleware.RequestMetrics(s.metrics)(handler)
	handler = middleware.RequestLogger(s.logger)(handler)

	s.obsServer = &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		s.logger.InfoContext(ctx, "http: listening", slog.String("addr", s.cfg.HTTPAddr))
		if err := s.obsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.ErrorContext(ctx, "http: server error", slog.Any("error", err))
		}
	}()

	// ── mDNS responder ────────────────────────────────────────────────────
	// TODO(tasks 89–90): advertise _sqi._tcp on the local network.
	s.logger.DebugContext(ctx, "discovery: not yet started (tasks 89–90)")

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
	// TODO(tasks 89–90): unregister mDNS service.

	// ── HTTP server ───────────────────────────────────────────────────────
	if s.obsServer != nil {
		if err := s.obsServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("http shutdown: %w", err))
		}
	}

	// ── Scheduler ─────────────────────────────────────────────────────────
	// TODO(tasks 46–55): signal scheduler to stop; wait for goroutines.

	// ── Message bus ───────────────────────────────────────────────────────
	// Tasks 33–35: shut down embedded NATS server.
	// TODO(tasks 36–39): drain in-flight consumers before calling Shutdown.
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
