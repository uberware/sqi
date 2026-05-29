// SPDX-License-Identifier: AGPL-3.0-only

// Package server owns the sqi-server component lifecycle: starting and
// stopping the store, message bus, scheduler, HTTP server, and mDNS
// responder in the correct dependency order.
//
// Components are added to [Server] as their implementing tasks land:
//   - store (SQLite):          tasks 25–32
//   - bus (NATS JetStream):    tasks 33–39
//   - scheduler:               tasks 46–55
//   - httpServer (REST+WS+UI): tasks 66–88
//   - discovery (mDNS):        tasks 89–90
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
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

	// NATSAddr is the TCP address the embedded NATS server listens on.
	// It defaults to loopback so external clients cannot reach it directly;
	// the sqi-server communicates with NATS in-process.
	NATSAddr string // default "127.0.0.1:4222"

	// NATSDataDir is the directory used by JetStream for file-backed stream
	// storage. It is created at startup if it does not exist.
	NATSDataDir string // default "data/nats"

	// SQLitePath is the path to the SQLite database file used in simple mode.
	// It is created at startup if it does not exist.
	SQLitePath string // default "sqi.db"
}

// DefaultConfig returns a [Config] with sensible development defaults.
// Production deployments override these via the config file or environment
// variables (tasks 16–19).
func DefaultConfig() Config {
	return Config{
		HTTPAddr:    "0.0.0.0:8080",
		NATSAddr:    "127.0.0.1:4222",
		NATSDataDir: "data/nats",
		SQLitePath:  "sqi.db",
	}
}

// Server coordinates the startup and shutdown of all sqi-server components.
// It is created by [New] and driven by [Run].
type Server struct {
	cfg    Config
	logger *slog.Logger

	// Component fields are added here as their tasks land.
	// store     *store.Store           // tasks 25–32
	// bus       *bus.Client            // tasks 33–39
	// scheduler *scheduler.Scheduler   // tasks 46–55
	// http      *http.Server           // tasks 66–88
	// discovery *discovery.Responder   // tasks 89–90
}

// New creates a [Server] with the given configuration and logger.
func New(cfg Config, logger *slog.Logger) *Server {
	return &Server{
		cfg:    cfg,
		logger: logger,
	}
}

// Run starts all server components and blocks until ctx is cancelled (typically
// by a SIGINT or SIGTERM delivered via [signal.NotifyContext]). It then
// initiates a graceful shutdown and returns once all components have stopped or
// [ShutdownTimeout] is exceeded.
//
// A nil return value means the server started and shut down without errors.
// Startup failures return immediately with a non-nil error.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Info("sqi-server starting",
		slog.String("http_addr", s.cfg.HTTPAddr),
		slog.String("nats_addr", s.cfg.NATSAddr),
		slog.String("sqlite_path", s.cfg.SQLitePath),
	)

	if err := s.start(ctx); err != nil {
		return fmt.Errorf("startup failed: %w", err)
	}

	s.logger.Info("sqi-server ready — waiting for signal")

	// Block until the calling context is cancelled.
	<-ctx.Done()

	s.logger.Info("shutdown signal received, stopping components")
	return s.shutdown()
}

// start brings up all components in dependency order.
// Each block is a stub that will be replaced as the corresponding tasks land.
func (s *Server) start(_ context.Context) error {
	// ── Store (SQLite) ─────────────────────────────────────────────────────
	// TODO(tasks 25–32): open store, run pending migrations.
	s.logger.Debug("store: not yet started (tasks 25–32)")

	// ── Message bus (NATS JetStream) ───────────────────────────────────────
	// TODO(tasks 33–39): embed and start NATS, configure JetStream streams.
	s.logger.Debug("bus: not yet started (tasks 33–39)")

	// ── Scheduler ─────────────────────────────────────────────────────────
	// TODO(tasks 46–55): start assignment loop, heartbeat sweep.
	s.logger.Debug("scheduler: not yet started (tasks 46–55)")

	// ── HTTP server (REST + WebSocket + embedded UI) ───────────────────────
	// TODO(tasks 66–88): bind listener, register routes, serve.
	s.logger.Debug("http: not yet started (tasks 66–88)")

	// ── mDNS responder ────────────────────────────────────────────────────
	// TODO(tasks 89–90): advertise _sqi._tcp on the local network.
	s.logger.Debug("discovery: not yet started (tasks 89–90)")

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
	// TODO(tasks 66–88): httpServer.Shutdown(ctx) — drains active connections.

	// ── Scheduler ─────────────────────────────────────────────────────────
	// TODO(tasks 46–55): signal scheduler to stop; wait for goroutines.

	// ── Message bus ───────────────────────────────────────────────────────
	// TODO(tasks 33–39): drain in-flight NATS messages, shutdown server.

	// ── Store ─────────────────────────────────────────────────────────────
	// TODO(tasks 25–32): flush WAL checkpoint, close SQLite connection.

	if ctx.Err() != nil {
		errs = append(errs, fmt.Errorf("graceful shutdown timed out after %s", ShutdownTimeout))
	}

	if len(errs) == 0 {
		s.logger.Info("sqi-server stopped cleanly")
		return nil
	}

	return errors.Join(errs...)
}
