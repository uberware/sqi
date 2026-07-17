// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server owns the sqi-server component lifecycle: starting and
// stopping the store, message bus, scheduler, HTTP server, and mDNS
// responder in the correct dependency order.
//
// The server owns these components, started and stopped in dependency order:
//   - store (SQLite)
//   - bus (NATS JetStream)
//   - scheduler
//   - httpServer (chi router) — REST + WebSocket + UI routes
//   - discovery (mDNS)
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/diag"
	"github.com/uberware/sqi/internal/discovery"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/presetlib"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/version"
	"github.com/uberware/sqi/internal/ws"
)

// ShutdownTimeout is the maximum time [Server.Run] waits for all components
// to stop cleanly before returning. Components that do not respect context
// cancellation within this window are abandoned.
const ShutdownTimeout = 30 * time.Second

// Config holds runtime parameters for the server.
//
// This struct is a stand-in used by the serve subcommand; the serve subcommand
// can also load a [config.Config] and derive a [Config] from it rather than
// using defaults.
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
	// It defaults to all interfaces so that workers discovering the server
	// via mDNS can connect to NATS at the advertised LAN host. (No broker
	// authentication yet; that arrives in phase 3.)
	NATSAddr string // default "0.0.0.0:4222"

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

	// DiscoveryEnabled controls whether the mDNS responder advertises this
	// server on the local network. Disable in environments that forbid
	// multicast (most cloud VPCs). Default true.
	DiscoveryEnabled bool

	// DiscoveryInstanceName is the mDNS service instance name advertised on
	// the network. Each server on the same subnet should use a distinct name.
	// Default "sqi-server".
	DiscoveryInstanceName string

	// DisableRateLimit turns off per-IP API rate limiting. Only set this in
	// tests and benchmarks; never in production.
	DisableRateLimit bool

	// EnforceOpenJDLimits controls whether quantitative OpenJD limit checks
	// (name lengths, element counts, reserved-name rules) are enforced during
	// job submission.  Default true.  Mirror of config.OpenJDConfig.EnforceLimits.
	EnforceOpenJDLimits bool

	// PresetLibraryURL is the URL of the community preset library index JSON.
	// When empty the /api/v1/presets endpoints respond 503.
	// Mirror of config.PresetLibraryConfig.URL.
	PresetLibraryURL string

	// AuthEnabled mirrors config.AuthConfig.Enabled. When true, start()
	// bootstraps the first admin (if needed) and selects the session
	// authenticator instead of the anonymous one. Default false.
	AuthEnabled bool

	// AuthSessionTTL mirrors config.AuthConfig.Session.TTL: the absolute
	// lifetime of sessions minted by POST /auth/login. Only consulted when
	// AuthEnabled is true.
	AuthSessionTTL time.Duration

	// AuthCookieName mirrors config.AuthConfig.Session.CookieName: the
	// session cookie name read by the session authenticator and set/cleared
	// by the login/logout handlers. Only consulted when AuthEnabled is true.
	AuthCookieName string

	// AuthCookieSecure mirrors config.AuthConfig.Session.CookieSecure: one of
	// "auto", "true", or "false". Only consulted when AuthEnabled is true.
	AuthCookieSecure string

	// AuthBootstrapUsername mirrors config.AuthConfig.Bootstrap.Username: the
	// username for the first admin, seeded once on an empty users table.
	AuthBootstrapUsername string

	// AuthBootstrapPassword mirrors config.AuthConfig.Bootstrap.Password: the
	// plaintext password for the first admin. Never logged; hashed with
	// password.Hash before it reaches the store.
	AuthBootstrapPassword string

	// SeedDefaults, when true, creates a "default" farm and queue on first
	// startup if the store has no farms yet. No-op once any farm exists.
	// Default true.
	SeedDefaults bool
}

// DefaultConfig returns a [Config] with sensible development defaults.
// Production deployments override these via the config file or environment
// variables.
func DefaultConfig() Config {
	return Config{
		HTTPAddr:              "0.0.0.0:8080",
		NATSAddr:              "0.0.0.0:4222",
		NATSDataDir:           "data/nats",
		NATSMaxStoreMB:        1024,
		SQLitePath:            "sqi.db",
		CheckpointInterval:    5 * time.Minute,
		Scheduler:             scheduler.DefaultConfig(),
		DiscoveryEnabled:      true,
		DiscoveryInstanceName: "sqi-server",
		EnforceOpenJDLimits:   true,
		SeedDefaults:          true,
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
	// (/metrics, /healthz, /readyz, /debug/pprof).
	httpServer *http.Server

	store     store.Store
	broker    *bus.Broker
	busClient *bus.Client // typed wrapper; drained before broker shutdown
	sched     *scheduler.Scheduler
	wsHub     *ws.Hub              // WebSocket fan-out hub
	discovery *discovery.Responder // mDNS advertisement

	// diagBuf is the in-memory diagnostic-log ring buffer. It is created in the
	// serve command before the logger (so the server's own logs are captured
	// from the first line) and threaded here. Nil when diagnostics are disabled.
	diagBuf *diag.Buffer
}

// New creates a [Server] with the given configuration and logger.
//
// diagBuf is the diagnostic-log ring buffer fed by the server's own logs (via
// the logger's server sink) and by worker diagnostics (via the scheduler). It
// must be created before the logger so startup logs are captured; pass nil to
// disable diagnostics.
func New(cfg Config, logger *slog.Logger, diagBuf *diag.Buffer) *Server {
	return &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: metrics.New(),
		health:  health.NewRegistry(),
		diagBuf: diagBuf,
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

	// Seed a default farm and queue on first startup so a fresh deployment can
	// accept job submissions without manual setup (no-op once any farm exists).
	if err := seedDefaults(ctx, s.store, s.cfg, s.logger); err != nil {
		return fmt.Errorf("seed defaults: %w", err)
	}

	// ── Message bus (NATS JetStream) ───────────────────────────────────────
	// Embed NATS server, enable JetStream, provision streams.
	// Typed client wrapper, consumers, reconnect, drain.
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

	// ── WebSocket hub ────────────────────────────────────────
	// The hub bridges scheduler events to subscribed WebSocket clients.
	// It is created before the scheduler so it can be passed as the notifier.
	s.wsHub = ws.NewHub(s.logger)
	s.logger.InfoContext(ctx, "ws: hub created")

	// Wire the diagnostic buffer's notifier to the hub now that the hub exists,
	// so every appended record (server logs and worker diagnostics) is fanned
	// out to subscribed WebSocket clients. No-op when diagnostics are disabled.
	if s.diagBuf != nil {
		hub := s.wsHub
		s.diagBuf.SetNotify(func(r diag.Record) {
			hub.NotifyDiag(ws.DiagEvent{
				Component: r.Component,
				Level:     r.Level,
				Msg:       r.Msg,
				Attrs:     r.Attrs,
				At:        r.Ts,
			})
		})
	}

	// ── Scheduler ─────────────────────────────────────────────────────────
	// Assignment loop goroutine pool, worker registry (NATS
	// consumer), and heartbeat timeout sweep.
	// The hub is passed as the notifier so live events are
	// pushed to subscribed WebSocket clients after each state change.
	s.sched = scheduler.New(s.cfg.Scheduler, s.store, s.busClient, s.metrics, s.logger, s.wsHub, s.diagBuf)
	go func() {
		if err := s.sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.ErrorContext(ctx, "scheduler: exited with error", slog.Any("error", err))
		}
	}()
	s.logger.InfoContext(ctx, "scheduler: started")

	// ── HTTP server (chi router) ──────────────────────────────────────────
	// Chi router with standard middleware mounts all routes.
	// Job REST endpoints are now registered via api.Deps.
	deps := api.Deps{
		Store: s.store,
		Submitter: openjd.NewSubmitterWithOptions(s.store, openjd.SubmitterOptions{
			EnforceLimits: s.cfg.EnforceOpenJDLimits,
		}),
		Products:  product.NewCatalog(s.store),
		Scheduler: s.sched,
		Hub:       s.wsHub,
		Version:   version.Get(),
	}
	// Only expose the diagnostics reader when diagnostics are enabled. Leaving
	// DiagReader as a nil interface (rather than a typed-nil *diag.Buffer) makes
	// the endpoint return 503 instead of panicking on Query.
	if s.diagBuf != nil {
		deps.DiagReader = s.diagBuf
	}
	// Only wire the preset library when a URL is configured. Leaving PresetLib
	// as a nil interface (rather than a typed-nil *presetlib.Service) makes the
	// endpoints return 503 instead of panicking.
	if s.cfg.PresetLibraryURL != "" {
		deps.PresetLib = presetlib.New(s.cfg.PresetLibraryURL, presetlib.DefaultCacheTTL)
	}
	deps.Auth, err = s.selectAuth(ctx)
	if err != nil {
		return fmt.Errorf("select auth: %w", err)
	}
	deps.SessionTTL = s.cfg.AuthSessionTTL
	deps.CookieName = s.cfg.AuthCookieName
	deps.CookieSecure = s.cfg.AuthCookieSecure
	router := api.NewRouter(
		api.Config{
			CORSOrigins:            s.cfg.CORSOrigins,
			EnablePprof:            s.cfg.EnablePprof,
			DisableRateLimit:       s.cfg.DisableRateLimit,
			WorkerOfflineThreshold: s.sched.WorkerTimeout(),
			AuthEnabled:            s.cfg.AuthEnabled,
		},
		deps,
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
		s.logger.InfoContext(ctx, "http: listening",
			slog.String("addr", s.cfg.HTTPAddr),
			slog.String("url", browseURL(s.cfg.HTTPAddr)))
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.ErrorContext(ctx, "http: server error", slog.Any("error", err))
		}
	}()

	// ── mDNS responder ────────────────────────────────────────────────────
	// Advertise _sqi._tcp on the local network so workers and the
	// sqi CLI can discover this server without manual address configuration.
	// Advertisement is gated on DiscoveryEnabled for environments
	// that forbid multicast.
	resp, err := discovery.New(discovery.Config{
		Enabled:      s.cfg.DiscoveryEnabled,
		InstanceName: s.cfg.DiscoveryInstanceName,
		HTTPAddr:     s.cfg.HTTPAddr,
		NATSAddr:     s.cfg.NATSAddr,
	}, s.logger)
	if err != nil {
		return fmt.Errorf("init discovery: %w", err)
	}
	if err := resp.Start(ctx); err != nil {
		return fmt.Errorf("start discovery: %w", err)
	}
	s.discovery = resp

	return nil
}

// selectAuth chooses the authenticator wired into the HTTP router. When auth
// is disabled it returns the anonymous superuser authenticator unchanged
// (auth-off must remain byte-for-byte pre-A1 behavior — no bootstrap runs, no
// store write happens). When auth is enabled it first bootstraps the initial
// admin (a no-op once any user exists) and then returns a session-cookie
// authenticator backed by the store.
func (s *Server) selectAuth(ctx context.Context) (auth.Authenticator, error) {
	if !s.cfg.AuthEnabled {
		return auth.Anonymous(), nil
	}
	if err := bootstrapAdmin(ctx, s.store, BootstrapParams{
		Username: s.cfg.AuthBootstrapUsername,
		Password: s.cfg.AuthBootstrapPassword,
	}, s.logger); err != nil {
		return nil, fmt.Errorf("auth bootstrap: %w", err)
	}
	return session.New(s.store, s.cfg.AuthCookieName, nil), nil
}

// browseURL turns a TCP bind address into a URL a human can paste into a
// browser. A server binds to the wildcard host "0.0.0.0" (all IPv4 interfaces)
// or "::" (all IPv6 interfaces), but those are not connectable addresses:
// Chrome silently rewrites "0.0.0.0" to localhost, while Safari refuses it and
// jumps to about:blank. To avoid that confusion we substitute "localhost" for
// any wildcard or empty host, leaving an explicit host (e.g. "127.0.0.1" or a
// LAN IP) untouched. The original bind address is still logged separately under
// the "addr" key for operators.
func browseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not in host:port form; surface it as-is rather than guessing.
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// shutdown stops all running components in reverse dependency order within
// a [ShutdownTimeout] deadline.
func (s *Server) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()

	var errs []error

	// Stop in reverse startup order.

	// ── mDNS responder ────────────────────────────────────────────────────
	// Stop advertising first so browsers see the goodbye packets and drop the
	// service promptly, rather than waiting for the record to expire.
	if s.discovery != nil {
		s.discovery.Shutdown()
	}

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
	// Drain the typed client first — stops push consumers,
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
