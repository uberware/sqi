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
	"slices"
	"time"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/apikey"
	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/auth/oidc"
	"github.com/uberware/sqi/internal/auth/rolemap"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/config"
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
	// via mDNS can connect to NATS at the advertised LAN host. (Broker
	// authentication does not exist: any host that can reach this port can
	// register as a worker and receive assignments. Deferred to Phase 4.)
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

	// OpenJDExprLimits bounds what one submitted template may spend inside the
	// EXPR expression checker — EXPR sub-project E4d. Mirror of
	// config.OpenJDConfig's four expr_* keys, mapped in cmd/sqi-server.
	//
	// The ZERO VALUE means "use openjd's defaults", so a Config built without
	// it (every test in this repo that constructs a server.Config literal)
	// behaves exactly as it did before this field existed.
	OpenJDExprLimits openjd.ExprLimits

	// PresetLibraryURL is the URL of the community preset library index JSON.
	// When empty the /api/v1/presets endpoints respond 503.
	// Mirror of config.PresetLibraryConfig.URL.
	PresetLibraryURL string

	// AuthEnabled mirrors config.AuthConfig.Enabled. When true, start()
	// bootstraps the first admin (if needed) and selects the session
	// authenticator instead of the anonymous one. Default false.
	AuthEnabled bool

	// AuthValidateJobOwner mirrors config.AuthConfig.ValidateJobOwner: when
	// true, a submit-as owner override must name a known user, else 400.
	// Default true.
	AuthValidateJobOwner bool

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

	// AuthLDAP mirrors config.AuthConfig.LDAP. Carried as a nested struct
	// rather than flattened into Auth* fields like its siblings: the block
	// has seventeen fields, and flattening them would bury the rest of this
	// struct. Only consulted when AuthEnabled is true.
	AuthLDAP config.LDAPConfig

	// AuthOIDC mirrors config.AuthConfig.OIDC, carried whole for the same
	// reason as AuthLDAP above. Only consulted when AuthEnabled is true.
	AuthOIDC config.OIDCConfig

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
		OpenJDExprLimits:      openjd.DefaultExprLimits(),
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

	// Reap expired sessions (no-op when auth is disabled).
	s.startSessionSweeper(ctx)

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
	s.wsHub = newWSHub(s.logger, st, s.cfg.AuthEnabled)
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
	s.sched = scheduler.New(schedulerConfig(s.cfg), s.store, s.busClient, s.metrics, s.logger, s.wsHub, s.diagBuf)
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
			ExprLimits:    s.cfg.OpenJDExprLimits,
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
	if err := s.wireAuthDeps(ctx, &deps); err != nil {
		return err
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
			ValidateJobOwner:       s.cfg.AuthValidateJobOwner,
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

// wsJobOwnerResolverTimeout bounds each individual wsJobOwnerResolver lookup;
// it does not bound the aggregate time spent across many lookups. The query
// is a single indexed primary-key read (jobs.id), so this is generous
// headroom rather than a tuned budget — its purpose is only to guarantee each
// call returns, since it runs synchronously on the calling scheduler
// goroutine inside NotifyTask/NotifyJob and an unbounded context.Background()
// call would block that goroutine indefinitely if the store ever hung.
// Failed resolutions are never cached (see ownerCache.get), so during a
// sustained store outage every single event pays up to this timeout, not
// just one — this constant caps the per-call cost, not the total stall an
// outage can accumulate across many events. newWSHub avoids that cost
// entirely when auth is disabled (no resolver, no store calls at all), and
// bounds the one-time-per-job lookup when auth is enabled.
const wsJobOwnerResolverTimeout = 2 * time.Second

// wsJobOwnerResolver returns a jobID → owner lookup for [ws.NewHub], backed by
// st. Job ownership is immutable, so the hub caches successful results —
// including an empty owner, which is what every job submitted before auth was
// enabled carries.
//
// The error is propagated rather than collapsed into an empty owner precisely
// so the hub can tell those apart: "this job has no owner" is a permanent fact
// worth caching, while "the store did not answer" must not be, or one timeout
// would hide a job from scoped clients for the process's lifetime. A lookup
// failure (including a not-found job and a timeout) fails closed.
func wsJobOwnerResolver(st store.Store) func(jobID string) (string, error) {
	return func(jobID string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), wsJobOwnerResolverTimeout)
		defer cancel()
		job, err := st.GetJob(ctx, jobID)
		if err != nil {
			return "", err
		}
		return job.Owner, nil
	}
}

// newWSHub constructs the WebSocket hub used by (*Server).start. Extracted to
// a named function (rather than inlined at the ws.NewHub call site) so this
// wiring is directly testable without booting the full server: a regression
// that silently drops owner scoping would otherwise be invisible to every
// test in this package.
//
// authEnabled drives both hub options together. With auth off, every client
// registers Scope{All: true} (readLoop's scopeFilter returns scoped=false for
// the anonymous superuser), so there is nothing to scope and no reason to
// resolve owners — and the job-summary paths fall back to their pre-B2
// hasSubscribers guard, keeping the auth-off hot path free of marshaling and
// ring-mutex traffic when nobody is connected.
func newWSHub(logger *slog.Logger, st store.Store, authEnabled bool) *ws.Hub {
	if !authEnabled {
		return ws.NewHub(logger, ws.HubOptions{})
	}
	return ws.NewHub(logger, ws.HubOptions{
		OwnerScoping: true,
		JobOwner:     wsJobOwnerResolver(st),
	})
}

// wireAuthDeps selects the request authenticator and, when configured,
// constructs the directory verifier, assigning both into deps. Split out of
// start so that the LDAP-specific error branch does not push start over its
// complexity budget; the two are still sequenced together here because both
// gate on AuthEnabled and both must succeed before the router is built.
func (s *Server) wireAuthDeps(ctx context.Context, deps *api.Deps) error {
	a, err := s.selectAuth(ctx)
	if err != nil {
		return fmt.Errorf("select auth: %w", err)
	}
	deps.Auth = a

	v, err := s.buildLDAPVerifier(ctx)
	if err != nil {
		return fmt.Errorf("auth ldap: %w", err)
	}
	deps.LDAPVerifier = v
	deps.LDAPConfig = toLDAPConfig(s.cfg.AuthLDAP, s.logger)

	p, key, err := s.buildOIDCProvider(ctx)
	if err != nil {
		return fmt.Errorf("auth oidc: %w", err)
	}
	deps.OIDCProvider = p
	deps.OIDCStateKey = key
	deps.OIDCConfig = toOIDCConfig(s.cfg.AuthOIDC, s.logger)
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
	// Compose credential types: a machine's Bearer API key leads, the
	// browser session cookie is the fallback. Both back onto the store.
	keyAuthn := apikey.New(s.store, nil)
	sessAuthn := session.New(s.store, s.cfg.AuthCookieName, nil)
	return auth.Chain(keyAuthn, sessAuthn), nil
}

// buildLDAPVerifier constructs the directory verifier, or nil when LDAP is
// disabled. A configuration fault (an unreadable CA bundle, say) is returned
// as an error so boot aborts: a server that accepted connections but could
// authenticate no directory account would look healthy while locking out
// every user it is supposed to serve.
func (s *Server) buildLDAPVerifier(ctx context.Context) (ldap.Verifier, error) {
	c := s.cfg.AuthLDAP
	if !s.cfg.AuthEnabled || !c.Enabled {
		return nil, nil
	}
	if c.TLSSkipVerify {
		// Loud on purpose: this disables certificate verification against the
		// directory, so a MITM can harvest every password that crosses it.
		s.logger.WarnContext(ctx, "auth: ldap TLS certificate verification is DISABLED (auth.ldap.tls_skip_verify)",
			slog.String("url", c.URL))
	}
	s.logger.InfoContext(
		ctx, "auth: ldap enabled",
		slog.String("url", c.URL),
		slog.Bool("template_bind", c.UserDNTemplate != ""),
		// Logged because flipping local -> directory silently overwrites
		// every manual role assignment as users log back in; the operator
		// deserves a record of which mode was live.
		slog.String("role_source", c.RoleSource),
		slog.Int("role_mappings", len(c.RoleMap)),
	)
	return ldap.New(toLDAPConfig(c, s.logger))
}

// toLDAPConfig converts the loader's config shape into the ldap package's,
// which is deliberately independent of internal/config. c.Enabled is not
// carried across: ldap.Config has no such field because the gate is applied
// here, before ldap.New is ever called, so the verifier type has no use for
// it.
func toLDAPConfig(c config.LDAPConfig, logger *slog.Logger) ldap.Config {
	out := ldap.Config{
		URL:             c.URL,
		StartTLS:        c.StartTLS,
		TLSSkipVerify:   c.TLSSkipVerify,
		CAFile:          c.CAFile,
		Timeout:         c.Timeout,
		BindDN:          c.BindDN,
		BindPassword:    c.BindPassword,
		BaseDN:          c.BaseDN,
		UserFilter:      c.UserFilter,
		NestedGroups:    c.NestedGroups,
		UserDNTemplate:  c.UserDNTemplate,
		UsernameAttr:    c.UsernameAttr,
		DisplayNameAttr: c.DisplayNameAttr,
		UniqueIDAttr:    c.UniqueIDAttr,
		RoleSource:      c.RoleSource,
		DefaultRole:     c.DefaultRole,
		RoleMap:         toRoleMap(c.RoleMap),
		Logger:          logger,
	}
	return out
}

// toRoleMap converts the loader's role-mapping list into the shared
// [rolemap.Mapping] form both auth providers consume. The two config blocks
// stay deliberately parallel, so this bridges both of them.
func toRoleMap(in []config.RoleMappingConfig) []rolemap.Mapping {
	out := make([]rolemap.Mapping, 0, len(in))
	for _, m := range in {
		out = append(out, rolemap.Mapping{Group: m.Group, Role: m.Role})
	}
	return out
}

// buildOIDCProvider constructs the SSO provider, or nil when SSO is disabled.
//
// Unlike buildLDAPVerifier this cannot fail on a network fault, because it
// performs no network I/O: discovery is lazy. That is deliberate. Discovery is
// an HTTP call to an external service, and a render farm's scheduler must not
// fail to start because the identity provider is briefly unreachable — that
// would make an external service a hard dependency of the whole system rather
// than of SSO alone. Configuration faults still abort boot, in Validate.
//
// The same line C1 draws: ldap.New assembles TLS at boot (so a bad ca_file
// aborts) but does not dial, so a downed domain controller does not stop
// startup.
//
// The returned key signs the per-login state cookie and is generated fresh on
// every boot, so a restart invalidates any login already in flight — an
// acceptable trade for never persisting it. It is returned alongside the
// provider rather than derived later because the router refuses to register
// the SSO routes without it: dropping it here produces a 404 on a deployment
// that believes it configured SSO.
func (s *Server) buildOIDCProvider(ctx context.Context) (oidc.Provider, []byte, error) {
	c := s.cfg.AuthOIDC
	if !s.cfg.AuthEnabled || !c.Enabled {
		return nil, nil, nil
	}
	key, err := oidc.NewSigningKey()
	if err != nil {
		return nil, nil, fmt.Errorf("oidc state key: %w", err)
	}
	s.logger.InfoContext(
		ctx, "auth: oidc enabled",
		slog.String("issuer", c.Issuer),
		// Logged for the same reason as LDAP's: flipping role_source to
		// directory means every login overwrites the account's role, and the
		// reauth/logout modes change what a logout actually guarantees. An
		// operator deserves a record of which modes were live.
		slog.String("role_source", c.RoleSource),
		slog.String("reauth_mode", c.ReauthMode),
		slog.String("logout_mode", c.LogoutMode),
		slog.Int("role_mappings", len(c.RoleMap)),
	)
	return oidc.New(toOIDCConfig(c, s.logger)), key, nil
}

// toOIDCConfig converts the loader's config shape into the oidc package's,
// which is deliberately independent of internal/config. As with toLDAPConfig,
// c.Enabled is not carried across: oidc.Config has no such field because the
// gate is applied here, before oidc.New is ever called.
func toOIDCConfig(c config.OIDCConfig, logger *slog.Logger) oidc.Config {
	out := oidc.Config{
		Issuer:                c.Issuer,
		ClientID:              c.ClientID,
		ClientSecret:          c.ClientSecret,
		RedirectURL:           c.RedirectURL,
		Scopes:                slices.Clone(c.Scopes),
		UsernameClaim:         c.UsernameClaim,
		DisplayNameClaim:      c.DisplayNameClaim,
		GroupsClaim:           c.GroupsClaim,
		RoleSource:            c.RoleSource,
		DefaultRole:           c.DefaultRole,
		ReauthMode:            c.ReauthMode,
		LogoutMode:            c.LogoutMode,
		PostLogoutRedirectURL: c.PostLogoutRedirectURL,
		ButtonLabel:           c.ButtonLabel,
		RoleMap:               toRoleMap(c.RoleMap),
		Logger:                logger,
	}
	return out
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

// sessionSweepInterval is how often expired sessions are reaped. Sessions
// expire on the order of days (config default: 168h), so the exact cadence
// does not matter much — this only has to be short enough that the table
// tracks the live session count rather than the all-time login count.
const sessionSweepInterval = time.Hour

// startSessionSweeper runs a periodic DeleteExpiredSessions until ctx is
// canceled. It sweeps once immediately so a server that is restarted more
// often than the interval still makes progress, rather than never reaching
// its first tick.
//
// Nothing else deletes a session on expiry — logout deletes by id, a password
// change deletes by user — so without this the sessions table grows by one row
// per login forever, enlarging the token_hash index that every authenticated
// request probes.
//
// It is a no-op when auth is disabled: no sessions are minted, so there is
// nothing to reap, and an auth-off server keeps exactly its pre-A1 behavior.
//
// Failures are logged and retried on the next tick: a sweep is pure
// housekeeping and must never take the server down.
func (s *Server) startSessionSweeper(ctx context.Context) {
	if !s.cfg.AuthEnabled {
		return
	}
	sweep := func() {
		n, err := s.store.DeleteExpiredSessions(ctx, time.Now().UTC())
		if err != nil {
			s.logger.ErrorContext(ctx, "auth: expired-session sweep failed", slog.Any("error", err))
			return
		}
		if n > 0 {
			s.logger.InfoContext(ctx, "auth: reaped expired sessions", slog.Int("count", n))
		}
	}

	go func() {
		ticker := time.NewTicker(sessionSweepInterval)
		defer ticker.Stop()
		sweep()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()

	s.logger.InfoContext(
		ctx, "auth: expired-session sweeper started",
		slog.Duration("interval", sessionSweepInterval),
	)
}
