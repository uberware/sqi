// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server owns the sqi-server component lifecycle: starting and
// stopping the store, message bus, scheduler, HTTP server, and mDNS
// responder in the correct dependency order.
//
// The server owns these components, started and stopped in dependency order:
//   - store (SQLite, or PostgreSQL when store.driver says so)
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
	"strings"
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
	"github.com/uberware/sqi/internal/pgdsn"
	"github.com/uberware/sqi/internal/presetlib"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/redact"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/tlsutil"
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

	// HTTPTLS terminates HTTPS in-process on the REST/WebSocket listener.
	// Zero value leaves it plaintext, which is the default.
	HTTPTLS config.TLSConfig

	// CORSOrigins is the list of origins that the CORS middleware permits.
	// Use ["*"] to allow all origins (suitable for local / dev deployments).
	// An empty slice is treated as ["*"]. Tighten this for production
	// deployments once the web UI's origin is known.
	CORSOrigins []string // default ["*"]

	// EnablePprof registers the Go runtime profiling endpoints at
	// /debug/pprof/ when true. Should never be enabled on servers accessible
	// to untrusted networks. Default false.
	EnablePprof bool

	// NATS carries the whole nats block: broker mode, worker authentication
	// and the embedded and external broker settings. The embedded broker's
	// address defaults to all interfaces so that workers discovering the
	// server via mDNS can connect at the advertised LAN host; broker
	// authentication is opt-in (NATS.Auth.Enabled) and off by default, and
	// warnIfBrokerUnauthenticated logs the exposure at startup.
	NATS config.NATSConfig

	// SQLitePath is the path to the SQLite database file used in simple mode.
	// It is created at startup if it does not exist.
	SQLitePath string // default "sqi.db"

	// CheckpointInterval is how often the background WAL checkpointer runs.
	// See internal/config.StoreConfig.CheckpointInterval for semantics.
	CheckpointInterval time.Duration // default 5m

	// StoreDriver is "sqlite" (default) or "postgres". An empty value means
	// sqlite.
	StoreDriver string

	// PostgresDSN and PostgresMaxConns configure the PostgreSQL store. They
	// are used only when StoreDriver is "postgres", in which case SQLitePath
	// and CheckpointInterval are not.
	PostgresDSN      string
	PostgresMaxConns int // default 16

	// Scheduler holds tuning parameters for the assignment loop, worker
	// registry, and heartbeat sweep. Zero values use scheduler.DefaultConfig().
	Scheduler scheduler.Config

	// DiscoveryEnabled controls whether the mDNS responder advertises this
	// server on the local network. Disable in environments that forbid
	// multicast (most cloud VPCs). Default true.
	DiscoveryEnabled bool

	// DiscoveryInterfaces restricts mDNS advertisement to specific interfaces.
	// Nil — the production value — advertises on all multicast-capable ones.
	// Set by tests to loopback; see internal/discovery.Config.Interfaces.
	DiscoveryInterfaces []net.Interface

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
	// EXPR expression checker. Mirror of config.OpenJDConfig's four expr_*
	// keys, mapped in cmd/sqi-server.
	//
	// The zero value means "use openjd's defaults", which is what every test
	// that constructs a server.Config literal without it gets.
	OpenJDExprLimits openjd.ExprLimits

	// OpenJDExprSubmissionDeadline is how long the expression checker may work
	// on one submission before this server gives up on it and answers 503 — the
	// EXPR wall-clock backstop. Mirror of
	// config.OpenJDConfig.ExprSubmissionDeadline, mapped in cmd/sqi-server.
	//
	// A duration, never an instant: the API layer turns it into an absolute
	// deadline per request. See openjd.SubmitOptions.Deadline for what goes
	// wrong if that conversion is hoisted anywhere that runs once.
	//
	// The zero value means no backstop, which is what every test that
	// constructs a server.Config literal without it gets.
	OpenJDExprSubmissionDeadline time.Duration

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

	// HA mirrors config.HAConfig. With HA.Enabled the server joins a
	// high-availability group: it starts as a warm standby and leads only
	// while it holds the leader lease (see ha.go). The zero value, as without
	// an ha section, is a single server.
	HA config.HAConfig
}

// DefaultConfig returns a [Config] with sensible development defaults.
// Production deployments override these via the config file or environment
// variables.
func DefaultConfig() Config {
	return Config{
		HTTPAddr:              "0.0.0.0:8080",
		NATS:                  config.DefaultConfig().NATS,
		SQLitePath:            "sqi.db",
		CheckpointInterval:    5 * time.Minute,
		StoreDriver:           backend.DriverSQLite,
		PostgresMaxConns:      16,
		Scheduler:             scheduler.DefaultConfig(),
		DiscoveryEnabled:      true,
		DiscoveryInstanceName: "sqi-server",
		EnforceOpenJDLimits:   true,
		OpenJDExprLimits:      openjd.DefaultExprLimits(),
		// Taken from internal/config's constant rather than restated, so this
		// default cannot drift from the one an operator's config file is
		// validated against. This package may import internal/config (it
		// already does, for the LDAP and OIDC blocks); internal/config may not
		// import internal/openjd, which is why the four expr_* LIMITS are
		// duplicated there and this is not.
		OpenJDExprSubmissionDeadline: config.DefaultOpenJDExprSubmissionDeadline,
		SeedDefaults:                 true,
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
	broker    bus.Broker
	busClient *bus.Client // typed wrapper; drained before broker shutdown
	sched     *scheduler.Scheduler
	wsHub     *ws.Hub   // WebSocket fan-out hub
	discovery responder // mDNS advertisement

	// ha is the high-availability state, nil without HA.
	ha *haState

	// cancelSched stops the scheduler's Run; set once it is started.
	cancelSched context.CancelFunc

	// wrapBroker, when set, wraps the broker newBroker built before it is
	// started. It is a test seam, set only by tests that observe start's
	// order through the broker; production leaves it nil.
	wrapBroker func(bus.Broker) bus.Broker

	// openBackend, when set, opens the store in place of [backend.Open], and
	// newResponder builds the mDNS responder in place of [discovery.New].
	// Test seams; production leaves both nil.
	openBackend  func(context.Context, backend.Options) (backend.Backend, error)
	newResponder func(discovery.Config, *slog.Logger) (responder, error)

	// checkpointDone closes after the SQLite WAL checkpointer's final TRUNCATE;
	// nil on a backend with no checkpointer.
	checkpointDone <-chan struct{}

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

// serveHTTP configures TLS on s.httpServer when it is enabled and starts the
// listener in a background goroutine. Split out of [Server.start] to keep that
// function under the cyclop complexity threshold.
func (s *Server) serveHTTP(ctx context.Context) error {
	// Both TLS surfaces are warned about here, beside the listener they concern,
	// rather than riding on broker startup where a reordering would silently
	// take the HTTP warning with it.
	warnTLSConfig(ctx, s.logger, s.cfg.HTTPTLS, s.cfg.NATS)

	tlsOn := s.cfg.HTTPTLS.Enabled
	if tlsOn {
		tlsCfg, err := tlsutil.ServerConfig(s.cfg.HTTPTLS.CertFile, s.cfg.HTTPTLS.KeyFile, "")
		if err != nil {
			return fmt.Errorf("http: tls: %w", err)
		}
		s.httpServer.TLSConfig = tlsCfg
	}

	go func() {
		s.logger.InfoContext(ctx, "http: listening",
			slog.String("addr", s.cfg.HTTPAddr),
			slog.Bool("tls", tlsOn),
			slog.String("url", browseURL(s.cfg.HTTPAddr, tlsOn)))

		// Certificates are already loaded into TLSConfig, so both paths pass
		// empty filenames: re-reading them here could pick up a half-written
		// file if a rotation lands between validation and listen.
		var err error
		if tlsOn {
			err = s.httpServer.ListenAndServeTLS("", "")
		} else {
			err = s.httpServer.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.ErrorContext(ctx, "http: server error", slog.Any("error", err))
		}
	}()
	return nil
}

// warnTLSConfig emits the advisory startup warnings for both TLS blocks:
// certificates staged but not switched on, and certificates close to expiry.
//
// Neither is a load error — staging a certificate before flipping the switch
// is a legitimate rollout step, and a certificate 29 days from expiry still
// works — but both are worth saying out loud once at startup. The broker's
// warnings are emitted here rather than in internal/bus so that package does
// not have to import internal/config. In external mode there is no embedded
// listener, so nats.embedded.tls is inert and not checked; the certificate
// that matters is the client one in nats.external.tls, which a cluster
// requiring mTLS refuses once it expires.
func warnTLSConfig(ctx context.Context, logger *slog.Logger, httpTLS config.TLSConfig, nats config.NATSConfig) {
	type surface struct {
		component string // log prefix
		key       string // the config key an operator would flip
		enabled   bool
		certFile  string
		keyFile   string
	}
	broker := surface{"bus", "nats.embedded.tls.enabled", nats.Embedded.TLS.Enabled, nats.Embedded.TLS.CertFile, nats.Embedded.TLS.KeyFile}
	if nats.Mode == config.NATSModeExternal {
		// A client certificate has no switch: it is in use when set.
		c := nats.External.TLS
		broker = surface{"bus", "", c.CertFile != "", c.CertFile, c.KeyFile}
	}
	surfaces := []surface{
		{"http", "http.tls.enabled", httpTLS.Enabled, httpTLS.CertFile, httpTLS.KeyFile},
		broker,
	}
	for _, s := range surfaces {
		if !s.enabled {
			if s.certFile != "" || s.keyFile != "" {
				logger.WarnContext(ctx,
					s.component+": tls certificates are configured but "+s.key+" is false; serving plaintext",
					slog.String("cert_file", s.certFile))
			}
			continue
		}
		if notAfter, soon := config.ExpiringSoon(s.certFile); soon {
			logger.WarnContext(ctx, s.component+": tls certificate expires soon",
				slog.String("cert_file", s.certFile), slog.Time("not_after", notAfter))
		}
	}
}

// warnIfBrokerUnauthenticated emits a WARN when the NATS broker has no
// credential requirement and may be reachable from outside this machine.
//
// This is the highest-value line in the whole broker-auth component: broker
// auth is opt-in and off by default, so this is the only thing that tells an
// operator who turned on auth.enabled that their worker transport is still
// wide open.
//
// For the embedded broker it stays silent on loopback, where the exposure does
// not exist, and silent on an unparseable address, where config validation
// has already produced a better message. For an external broker it always
// warns: sqi cannot tell who else can reach an operator-run cluster.
func warnIfBrokerUnauthenticated(ctx context.Context, n config.NATSConfig, logger *slog.Logger) {
	if n.Auth.Enabled {
		return
	}
	if n.Mode == config.NATSModeExternal {
		logger.WarnContext(
			ctx, "the NATS broker is unauthenticated",
			slog.String("nats_urls", redact.URLs(n.External.URLs, ",")),
			slog.String("impact", "any host that can reach the broker can register as a worker and execute submitted job code"),
			slog.String("remediation", "set nats.auth.enabled: true, configure nats.external.callout and enroll your workers"),
		)
		return
	}
	natsAddr := n.Embedded.Addr
	host, _, err := net.SplitHostPort(natsAddr)
	if err != nil {
		return
	}
	if isLoopbackHost(host) {
		return
	}
	logger.WarnContext(
		ctx, "the NATS broker is unauthenticated and reachable beyond this host",
		slog.String("nats_addr", natsAddr),
		slog.String("impact", "any host that can reach this port can register as a worker and execute submitted job code"),
		slog.String("remediation", "set nats.auth.enabled: true and enroll your workers, or bind nats.embedded.addr to 127.0.0.1:4222 for single-machine use"),
	)
}

// warnIfRevocationBounded emits a WARN when worker revocation on an external
// broker cannot be immediate: with authentication on and no system
// credential, sqi-server cannot reach a revoked worker's live connection, so
// it lasts until its user JWT expires.
func warnIfRevocationBounded(ctx context.Context, n config.NATSConfig, logger *slog.Logger) {
	if n.Mode != config.NATSModeExternal || !n.Auth.Enabled || n.External.Callout.SystemCredsFile != "" {
		return
	}
	logger.WarnContext(
		ctx, "worker revocation takes effect within "+shortDuration(n.Auth.UserTTL)+" on the external NATS broker",
		slog.String("impact", "a revoked worker stays connected until its user JWT expires (nats.auth.user_ttl); its reconnect is then refused"),
		slog.String("remediation", "set nats.external.callout.system_creds_file to a system-account user's nkey seed to make revocation immediate"),
	)
}

// shortDuration formats d without trailing zero units: one hour is "1h",
// 90 minutes "1h30m".
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// natsStartAttr names the broker in the startup line: the embedded broker's
// listen address, or the external broker's URLs.
func natsStartAttr(n config.NATSConfig) slog.Attr {
	if n.Mode == config.NATSModeExternal {
		return slog.String("nats_urls", redact.URLs(n.External.URLs, ","))
	}
	return slog.String("nats_addr", n.Embedded.Addr)
}

// isLoopbackHost reports whether host names only this machine. An empty host
// (from ":4222") means all interfaces, which is not loopback.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// startBroker constructs and starts the broker nats.mode selects, warning
// about an unauthenticated broker that may be exposed beyond this host. With
// broker authentication on, the broker reads each connecting worker's
// credential from the store through the auth callout, so nothing is loaded
// here.
func (s *Server) startBroker(ctx context.Context) (bus.Broker, error) {
	broker, err := s.buildBroker(ctx)
	if err != nil {
		return nil, err
	}
	if err := broker.Start(ctx); err != nil {
		return nil, err
	}
	return broker, nil
}

// buildBroker constructs the broker nats.mode selects without starting it,
// warning about an unauthenticated broker that may be exposed beyond this
// host.
func (s *Server) buildBroker(ctx context.Context) (bus.Broker, error) {
	warnIfBrokerUnauthenticated(ctx, s.cfg.NATS, s.logger)
	warnIfRevocationBounded(ctx, s.cfg.NATS, s.logger)

	broker, err := s.newBroker()
	if err != nil {
		return nil, err
	}
	if s.wrapBroker != nil {
		broker = s.wrapBroker(broker)
	}
	return broker, nil
}

// revokeBrokerTimeout is the least time [Server.RevokeWorker] allows the
// broker's part. The broker needs about its auth timeout plus a margin
// (2.25s embedded, 3.25s on an external cluster at nats-server's defaults);
// a broker whose [bus.Broker.RevokeBudget] says it needs longer, a cluster
// with a long auth timeout, gets that instead. Either way the bound stops a
// stuck broker from holding the request open indefinitely.
const revokeBrokerTimeout = 10 * time.Second

// RevokeWorker revokes workerID's active broker credential and, when broker
// authentication is enabled, disconnects it and reclaims its in-flight work.
// It implements [api.WorkerRevoker] and is what makes DELETE
// /api/v1/workers/{id}/credential the synchronous revocation path: the
// offline "sqi-server worker revoke" CLI command writes the same store row
// from a separate process holding no broker handle, so it takes effect at the
// worker's next connect; this method runs inside the server process where
// the broker handle lives, so it can disconnect the worker immediately.
//
// The store is written before the broker is called: once the row is
// revoked, the auth callout refuses the worker's reconnect, so the disconnect
// makes the revocation take effect immediately instead of at its user JWT's
// expiry. A broker failure after a successful store write leaves the worker
// connected until that expiry at the latest, and it is not trusted again
// afterwards. The failure is returned to the caller, since the synchronous
// disconnect did not happen, but the store write is never rolled back.
//
// The broker call runs on a context detached from ctx and bounded by the
// larger of [revokeBrokerTimeout] and the broker's own
// [bus.Broker.RevokeBudget]. [bus.Broker.Revoke] takes seconds, and its final
// pass is what closes the window for a connection whose authorization began
// before the revocation: neither an HTTP client disconnecting mid-request
// nor a bound shorter than the broker's auth timeout may cut that pass off.
func (s *Server) RevokeWorker(ctx context.Context, workerID string) error {
	if err := s.store.RevokeWorkerCredential(ctx, workerID, time.Now().UTC()); err != nil {
		return err
	}
	if !s.cfg.NATS.Auth.Enabled {
		return nil
	}
	if s.broker == nil {
		return errors.New("revoke worker: broker not started")
	}
	brokerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), max(revokeBrokerTimeout, s.broker.RevokeBudget()))
	defer cancel()
	if err := s.broker.Revoke(brokerCtx, workerID); err != nil {
		return fmt.Errorf("revoke worker: %w", err)
	}
	return nil
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
	driver := backend.Name(s.cfg.StoreDriver)
	attrs := []any{
		slog.String("http_addr", s.cfg.HTTPAddr),
		natsStartAttr(s.cfg.NATS),
		slog.String("store_driver", driver),
	}
	if driver == backend.DriverSQLite {
		// The path means nothing on another driver, and the DSN is logged
		// (redacted) when the store opens.
		attrs = append(attrs, slog.String("sqlite_path", s.cfg.SQLitePath))
	}
	s.logger.InfoContext(ctx, "sqi-server starting", attrs...)

	if s.cfg.HA.Enabled {
		return s.runHA(ctx)
	}

	if err := s.start(ctx); err != nil {
		s.abortStart()
		return fmt.Errorf("startup failed: %w", err)
	}

	s.logger.InfoContext(ctx, "sqi-server ready — waiting for signal")

	// Block until the calling context is canceled.
	<-ctx.Done()

	s.logger.InfoContext(ctx, "shutdown signal received, stopping components")
	return s.shutdown()
}

// openStore opens the configured state store, registers it as a readiness
// dependency under its driver's name, and, on SQLite, starts the WAL
// checkpointer.
func (s *Server) openStore(ctx context.Context) error {
	o := backend.Options{
		Driver: s.cfg.StoreDriver, SQLitePath: s.cfg.SQLitePath,
		PostgresDSN: s.cfg.PostgresDSN, MaxConns: s.cfg.PostgresMaxConns,
		AutoMigrate: true, ServerLock: true, Logger: s.logger,
	}
	if s.ha != nil {
		// Share the lock with the group's other servers, and bound sessions
		// by the lease.
		o.HA, o.LeaseTTL = true, s.cfg.HA.LeaseTTL
	}
	open := backend.Open
	if s.openBackend != nil {
		open = s.openBackend
	}
	st, err := open(ctx, o)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	s.store = st
	driver := backend.Name(s.cfg.StoreDriver)
	if driver == backend.DriverPostgres {
		s.logger.InfoContext(ctx, "store: postgres open", slog.String("dsn", pgdsn.Redact(s.cfg.PostgresDSN)))
	} else {
		s.logger.InfoContext(ctx, "store: sqlite open", slog.String("path", s.cfg.SQLitePath))
	}

	// Register the store as a readiness dependency under its driver's name, so
	// /readyz reports it as "sqlite" or "postgres".
	s.health.Register(driver, health.CheckerFunc(st.Ping))

	// SQLite only: start the background WAL checkpointer. It exits when ctx is
	// canceled, running one final checkpoint before returning so the WAL is
	// fully flushed before store.Close is called in shutdown.
	if cp, ok := st.(backend.Checkpointer); ok {
		s.checkpointDone = cp.StartCheckpointer(ctx, s.cfg.CheckpointInterval, s.logger)
		s.logger.InfoContext(
			ctx, "store: wal checkpointer started",
			slog.Duration("interval", s.cfg.CheckpointInterval),
		)
	}
	return nil
}

// start brings up all components in dependency order.
func (s *Server) start(ctx context.Context) error {
	// ── Store (SQLite or PostgreSQL) ───────────────────────────────────────
	if err := s.openStore(ctx); err != nil {
		return err
	}

	// Reap expired sessions (no-op when auth is disabled).
	s.startSessionSweeper(ctx)

	// Seed a default farm and queue on first startup so a fresh deployment can
	// accept job submissions without manual setup (no-op once any farm exists).
	if err := seedDefaults(ctx, s.store, s.cfg, s.logger); err != nil {
		return fmt.Errorf("seed defaults: %w", err)
	}

	// ── Message bus (NATS JetStream) ───────────────────────────────────────
	// Start the embedded broker, or connect to the external one, and
	// provision sqi's streams on it.
	broker, err := s.startBroker(ctx)
	if err != nil {
		return fmt.Errorf("start bus: %w", err)
	}
	s.broker = broker

	// A broker the database has not run against may hold messages for attempts
	// the store still calls open. Fence them before anything consumes.
	if err := s.reconcileBrokerLayout(ctx); err != nil {
		return fmt.Errorf("reconcile broker layout: %w", err)
	}

	// Register NATS as a readiness dependency so GET /readyz reflects its health.
	s.health.Register("nats", health.CheckerFunc(broker.Check))

	// Dial the broker with a typed client.  All server components
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
	s.wsHub = newWSHub(s.logger, s.store, s.cfg.AuthEnabled)
	s.logger.InfoContext(ctx, "ws: hub created")

	// Wire the diagnostic buffer's notifier to the hub now that the hub exists,
	// so every appended record (server logs and worker diagnostics) is fanned
	// out to subscribed WebSocket clients. No-op when diagnostics are disabled.
	s.notifyDiagToHub()

	// ── Scheduler ─────────────────────────────────────────────────────────
	// Assignment loop goroutine pool, worker registry (NATS
	// consumer), and heartbeat timeout sweep.
	// The hub is passed as the notifier so live events are
	// pushed to subscribed WebSocket clients after each state change.
	s.sched = scheduler.New(schedulerConfig(s.cfg, busClient), s.store, s.busClient, s.metrics, s.logger, s.wsHub, s.diagBuf)
	// The scheduler runs on its own child context so a failed start can stop
	// it (see abortStart) without racing Run's own setup of Scheduler.Stop.
	schedCtx, cancelSched := context.WithCancel(ctx)
	s.cancelSched = cancelSched
	go func() {
		if err := s.sched.Run(schedCtx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.ErrorContext(ctx, "scheduler: exited with error", slog.Any("error", err))
		}
	}()
	s.logger.InfoContext(ctx, "scheduler: started")

	// ── HTTP server (chi router) ──────────────────────────────────────────
	// Chi router with standard middleware mounts all routes.
	// Job REST endpoints are now registered via api.Deps.
	deps := s.apiDeps()
	if err := s.wireAuthDeps(ctx, &deps); err != nil {
		return err
	}
	router := api.NewRouter(
		routerConfig(s.cfg),
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

	if err := s.serveHTTP(ctx); err != nil {
		return err
	}

	return s.startDiscovery(ctx)
}

// notifyDiagToHub fans every record appended to the diagnostic buffer out to
// the WebSocket hub's subscribers. No-op when diagnostics are disabled.
func (s *Server) notifyDiagToHub() {
	if s.diagBuf == nil {
		return
	}
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

// apiDeps assembles the REST handlers' dependencies from what start (or an
// HA standby's buildStandbyAPI) has built, with the session and broker
// settings, short of authentication, which each path wires itself.
func (s *Server) apiDeps() api.Deps {
	deps := api.Deps{
		Store: s.store,
		Submitter: openjd.NewSubmitterWithOptions(s.store, openjd.SubmitterOptions{
			EnforceLimits: s.cfg.EnforceOpenJDLimits,
			ExprLimits:    s.cfg.OpenJDExprLimits,
		}),
		Products:      product.NewCatalog(s.store),
		Scheduler:     s.sched,
		Hub:           s.wsHub,
		Version:       version.Get(),
		Servers:       s.serversConfig(),
		WorkerRevoker: s,
		SessionTTL:    s.cfg.AuthSessionTTL,
		CookieName:    s.cfg.AuthCookieName,
		CookieSecure:  s.cfg.AuthCookieSecure,
	}
	natsAuthDeps(s.cfg, &deps)
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
	return deps
}

// responder is the mDNS responder as the server drives it.
type responder interface {
	Start(ctx context.Context) error
	Shutdown()
}

// newDiscoveryResponder is [discovery.New] as a [responder].
func newDiscoveryResponder(cfg discovery.Config, logger *slog.Logger) (responder, error) {
	r, err := discovery.New(cfg, logger)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// startDiscovery advertises _sqi._tcp on the local network so workers and the
// sqi CLI can discover this server without manual address configuration.
// Advertisement is gated on DiscoveryEnabled for environments that forbid
// multicast.
func (s *Server) startDiscovery(ctx context.Context) error {
	natsURL, err := s.advertisedNATSURL()
	if err != nil {
		return fmt.Errorf("init discovery: %w", err)
	}
	newResponder := s.newResponder
	if newResponder == nil {
		newResponder = newDiscoveryResponder
	}
	resp, err := newResponder(discovery.Config{
		Enabled:      s.cfg.DiscoveryEnabled,
		InstanceName: s.cfg.DiscoveryInstanceName,
		HTTPAddr:     s.cfg.HTTPAddr,
		NATSURL:      natsURL,
		HTTPTLS:      s.cfg.HTTPTLS.Enabled,
		Interfaces:   s.cfg.DiscoveryInterfaces,
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

// advertisedNATSURL is the broker URL discovery tells workers to use, or ""
// when discovery is off.
func (s *Server) advertisedNATSURL() (string, error) {
	if !s.cfg.DiscoveryEnabled {
		return "", nil // nothing is advertised, so nothing can be wrong with it
	}
	return s.broker.AdvertiseURL(discovery.SRVHost())
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
// resolve owners — and the job-summary paths fall back to their hasSubscribers
// guard, keeping the auth-off hot path free of marshaling and ring-mutex
// traffic when nobody is connected.
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
	return s.wireDirectoryDeps(ctx, deps)
}

// wireDirectoryDeps constructs the LDAP verifier and the SSO provider, when
// configured, and assigns them and their settings into deps.
func (s *Server) wireDirectoryDeps(ctx context.Context, deps *api.Deps) error {
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

// selectAuth chooses the authenticator wired into the HTTP router. When auth is
// disabled it returns the anonymous superuser authenticator unchanged (auth off
// must behave byte-for-byte as a server without auth — no bootstrap runs, no
// store write happens). When auth is enabled it first bootstraps the initial
// admin (a no-op once any user exists) and then returns a session-cookie
// authenticator backed by the store.
func (s *Server) selectAuth(ctx context.Context) (auth.Authenticator, error) {
	if !s.cfg.AuthEnabled {
		return auth.Anonymous(), nil
	}
	if err := s.bootstrapAuth(ctx); err != nil {
		return nil, err
	}
	return s.authenticator(), nil
}

// bootstrapAuth seeds the first admin when auth is enabled and the users
// table is empty. An HA server runs it at promotion rather than at boot:
// its count-then-create would race another server's.
func (s *Server) bootstrapAuth(ctx context.Context) error {
	if err := bootstrapAdmin(ctx, s.store, BootstrapParams{
		Username: s.cfg.AuthBootstrapUsername,
		Password: s.cfg.AuthBootstrapPassword,
	}, s.logger); err != nil {
		return fmt.Errorf("auth bootstrap: %w", err)
	}
	return nil
}

// authenticator is the request authenticator: the anonymous superuser with
// auth off, otherwise a Bearer API key or a session cookie, both backed by
// the store.
func (s *Server) authenticator() auth.Authenticator {
	if !s.cfg.AuthEnabled {
		return auth.Anonymous()
	}
	// Compose credential types: a machine's Bearer API key leads, the
	// browser session cookie is the fallback. Both back onto the store.
	keyAuthn := apikey.New(s.store, nil)
	sessAuthn := session.New(s.store, s.cfg.AuthCookieName, nil)
	return auth.Chain(keyAuthn, sessAuthn)
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
// LDAP draws the same line: ldap.New assembles TLS at boot (so a bad ca_file
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
func browseURL(addr string, tlsOn bool) string {
	scheme := "http://"
	if tlsOn {
		scheme = "https://"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not in host:port form; surface it as-is rather than guessing.
		return scheme + addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return scheme + net.JoinHostPort(host, port)
}

// abortStartTimeout bounds stopping the HTTP listener after a failed start.
const abortStartTimeout = 5 * time.Second

// abortStart stops what a failed [Server.start] had already started, so that
// an immediate retry finds nothing held. The broker matters most: an external
// broker holds its account's guard until it is shut down, and a retry inside
// the guard's lease would be refused naming this host. The store is left
// as it is; the process is exiting.
func (s *Server) abortStart() {
	if s.discovery != nil {
		s.discovery.Shutdown()
	}
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), abortStartTimeout)
		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.WarnContext(ctx, "http: shutdown after a failed start", slog.Any("error", err))
		}
		cancel()
	}
	if s.cancelSched != nil {
		s.cancelSched()
	}
	if s.busClient != nil {
		s.busClient.Close()
	}
	if s.broker != nil {
		// Bounded inside: an external broker gives its guard up within its
		// own guard-operation timeout.
		s.broker.Shutdown()
	}
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

	// ── HA leader lease ───────────────────────────────────────────────────
	// Hand the lease over before the HTTP drain, which can last as long as
	// its slowest request: a standby takes over at its next poll instead.
	if err := s.handOver(ctx); err != nil {
		errs = append(errs, err)
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
	// shut the broker down: stop the embedded server, or close sqi's
	// connections to the external one.
	if s.busClient != nil {
		if err := s.busClient.Drain(ctx); err != nil {
			errs = append(errs, fmt.Errorf("bus client drain: %w", err))
		}
	}
	if s.broker != nil {
		s.broker.Shutdown()
	}

	// SQLite: let the checkpointer's final TRUNCATE finish before the store
	// closes under it, bounded by the shutdown budget.
	s.waitCheckpoint(ctx)

	// HA: the membership and observer loops write to the store until they
	// stop.
	s.ha.stopLoops()

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

// waitCheckpoint waits for the SQLite checkpointer's final TRUNCATE, if there
// is a checkpointer, until ctx ends.
func (s *Server) waitCheckpoint(ctx context.Context) {
	if s.checkpointDone == nil {
		return
	}
	select {
	case <-s.checkpointDone:
	case <-ctx.Done():
	}
}

// sessionSweepInterval is how often expired sessions are reaped. Sessions
// expire on the order of days (config default: 168h), so the exact cadence
// does not matter much — this only has to be short enough that the table
// tracks the live session count rather than the all-time login count.
const sessionSweepInterval = time.Hour

// sweepEvery runs sweep every [sessionSweepInterval] until ctx ends, and
// once at the start too when now is set: the loop of the expired-session
// sweeper, and of an HA leader's sweeper, which prunes member rows too.
func sweepEvery(ctx context.Context, now bool, sweep func()) {
	ticker := time.NewTicker(sessionSweepInterval)
	defer ticker.Stop()
	if now {
		sweep()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

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
// nothing to reap, and an auth-off server does no auth work at all.
//
// Failures are logged and retried on the next tick: a sweep is pure
// housekeeping and must never take the server down.
func (s *Server) startSessionSweeper(ctx context.Context) {
	if !s.cfg.AuthEnabled {
		return
	}
	sweep := func() {
		if err := s.sweepSessions(ctx); err != nil {
			s.logger.ErrorContext(ctx, "auth: expired-session sweep failed", slog.Any("error", err))
		}
	}

	go sweepEvery(ctx, true, sweep)

	s.logger.InfoContext(
		ctx, "auth: expired-session sweeper started",
		slog.Duration("interval", sessionSweepInterval),
	)
}

// sweepSessions deletes the expired sessions once, logging how many it reaped.
func (s *Server) sweepSessions(ctx context.Context) error {
	n, err := s.store.DeleteExpiredSessions(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	if n > 0 {
		s.logger.InfoContext(ctx, "auth: reaped expired sessions", slog.Int("count", n))
	}
	return nil
}
