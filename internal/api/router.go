// SPDX-License-Identifier: AGPL-3.0-or-later

// Package api wires together the sqi-server HTTP surface: the chi router,
// standard middleware stack, and all route groups.
//
// # Router layout
//
//	GET /healthz — liveness probe
//	GET /readyz — readiness probe
//	GET /metrics — Prometheus scrape endpoint
//	GET /debug/pprof/* — Go runtime profiling (gated by config)
//	/api/v1/* — REST API
//	GET /api/v1/ws — WebSocket upgrade
//	GET /api/v1/openapi.yaml — OpenAPI 3.1 spec
//	/* — embedded SPA + static assets
//
// # Middleware stack (outermost → innermost)
//
//  1. chi Recoverer          — catches panics, returns 500, logs stack trace
//  2. go-chi/cors            — CORS preflight + headers (origins from Config)
//  3. chi RequestID          — sets X-Request-ID; chi's version generates a
//     UUID when the header is absent
//  4. chi Compress           — gzip response bodies for clients that accept it
//  5. middleware.RequestLogger — structured slog request logging
//  6. middleware.RequestMetrics — Prometheus HTTP metrics
//
// # /api/v1 sub-router middleware
//
//  7. middleware.APIVersion("1") — sets X-API-Version: 1 on every API response.
//     To mark individual endpoints as deprecated, wrap their handler with
//     middleware.Deprecated.
//
// Note: the structured-logging and metrics middleware are
// mounted here instead of the now-removed obsServer so every HTTP and
// WebSocket request continues to be logged and counted.
package api

import (
	"context"
	"log/slog"
	"net/http"
	httppprof "net/http/pprof"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/auth/oidc"
	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/middleware"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ui"
	"github.com/uberware/sqi/internal/version"
	"github.com/uberware/sqi/internal/ws"
)

// Config holds HTTP-layer configuration for [NewRouter].
type Config struct {
	// CORSOrigins is the list of origins the CORS middleware allows.
	// Use ["*"] to permit all origins (suitable for local/dev deployments).
	// Defaults to ["*"] when empty.
	CORSOrigins []string

	// EnablePprof registers Go runtime profiling endpoints at /debug/pprof/
	// when true. Must never be enabled on servers exposed to untrusted networks.
	EnablePprof bool

	// DisableRateLimit turns off per-IP rate limiting when true.
	// Use in tests and benchmarks where the loopback address would cause all
	// concurrent requests to share one bucket and trigger 429s.
	DisableRateLimit bool

	// WorkerOfflineThreshold is the heartbeat-timeout window used to decide
	// whether a disabled worker is dead (and thus removable). It mirrors the
	// scheduler's WorkerTimeout. Zero is treated as "no grace" (a disabled
	// worker with any past heartbeat is considered dead).
	WorkerOfflineThreshold time.Duration

	// AuthEnabled reflects config.AuthConfig.Enabled. When true, the session
	// cookie is in play and this router activates the browser-security
	// surface it requires: CORS AllowCredentials, the Origin-based CSRF guard
	// on the authenticated route group, and WebSocket Origin enforcement
	// (OriginPatterns instead of InsecureSkipVerify). When false, all three
	// are byte-for-byte unchanged from pre-A1 behavior.
	AuthEnabled bool

	// ValidateJobOwner mirrors config.AuthConfig.ValidateJobOwner: when true,
	// a submit-as owner override on POST /jobs and POST /products/{name}/jobs
	// must name a known user, else 400. Default true.
	ValidateJobOwner bool
}

// Deps holds the application-layer dependencies injected into the REST
// handlers. All fields are required; a nil field will cause a panic when the
// corresponding endpoint is first called.
type Deps struct {
	// Store provides access to all persistent state (jobs, tasks, workers, …).
	Store store.Store

	// Submitter handles the full OpenJD parse → validate → persist pipeline
	// used by POST /api/v1/jobs.
	Submitter *openjd.Submitter

	// Scheduler is the running scheduler instance. It is called by
	// DELETE /api/v1/jobs/{id} to propagate cancellation to workers
	// and by future task/retry endpoints.
	Scheduler *scheduler.Scheduler

	// Hub is the WebSocket fan-out hub used by the /api/v1/ws handler.
	// May be nil in tests that do not exercise WebSocket push;
	// the handler degrades gracefully by accepting subscriptions without
	// delivering any pushes.
	Hub *ws.Hub

	// DiagReader exposes the in-memory diagnostic log buffer to
	// GET /api/v1/diagnostics/logs. It is nil-tolerant: when nil (diagnostics
	// disabled) the endpoint responds with 503.
	DiagReader DiagReader

	// Products is the product catalog (built-ins + stored), used by the
	// /api/v1/products endpoints. Required for product routes.
	Products *product.Catalog

	// PresetLib is the community preset library client used by the
	// /api/v1/presets endpoints. Nil-tolerant: when nil or unconfigured the
	// endpoints respond 503.
	PresetLib PresetLibrary

	// Version is the server build metadata reported by GET /api/v1/version.
	// The zero value is acceptable (fields default to empty strings).
	Version version.Info

	// Auth authenticates each REST request and the WebSocket upgrade. Nil is
	// treated as auth.Anonymous() (auth disabled): every request is the
	// anonymous superuser principal and behavior is unchanged.
	Auth auth.Authenticator

	// SessionTTL is the absolute lifetime of sessions minted by
	// POST /api/v1/auth/login. Mirrors config.AuthConfig.Session.TTL.
	SessionTTL time.Duration

	// CookieName is the session cookie name set by login and cleared by
	// logout. Mirrors config.AuthConfig.Session.CookieName; must match the
	// name the Auth authenticator (e.g. session.Authenticator) reads.
	CookieName string

	// CookieSecure controls the session cookie's Secure attribute: "auto"
	// (Secure when the request is TLS or carries X-Forwarded-Proto: https),
	// "true", or "false". Mirrors config.AuthConfig.Session.CookieSecure.
	CookieSecure string

	// LDAPVerifier authenticates accounts whose AuthSource is
	// store.AuthSourceLDAP. Nil when directory auth is disabled, in which
	// case login behaves exactly as it did before C1.
	LDAPVerifier ldap.Verifier

	// LDAPConfig supplies the role mapping and role-source mode. Only read
	// when LDAPVerifier is non-nil.
	LDAPConfig ldap.Config

	// OIDCProvider authenticates SSO logins. Nil when SSO is disabled, in
	// which case the /auth/oidc/* routes are not registered at all.
	OIDCProvider oidc.Provider

	// OIDCConfig supplies claim names, role mapping, and the reauth/logout
	// modes. Only read when OIDCProvider is non-nil.
	OIDCConfig oidc.Config

	// OIDCStateKey signs the per-login state cookie. Generated per boot by
	// the server, never persisted: a restart invalidating in-flight logins is
	// cheaper than a key to configure, distribute, and rotate. Only read when
	// OIDCProvider is non-nil.
	OIDCStateKey []byte
}

// resolveCORSOrigins returns the CORS allow-list to configure, dropping the
// wildcard when auth is enabled.
//
// AllowCredentials + a wildcard origin is an invalid CORS configuration
// (browsers reject it) and, if honored, would let any origin ride the session
// cookie. We drop the wildcard rather than silently disabling credentials.
//
// Two situations reach that same drop, and they are NOT equally noteworthy —
// which is why they log at different levels:
//
//   - No origins configured at all. This is the default and it is correct: the
//     embedded web UI is same-origin and unaffected. Only a browser client
//     served from a different origin needs anything set. Informational.
//   - "*" configured explicitly. The operator asked for something that cannot
//     work with credentials, so their intent is not being honored. A warning.
//
// Reporting the default case as a failure ("dropping the wildcard") told
// first-time operators that enabling auth had broken something, when the
// wildcard was one they never asked for.
func resolveCORSOrigins(cfg Config, logger *slog.Logger) []string {
	origins := cfg.CORSOrigins
	defaulted := len(origins) == 0
	if defaulted {
		origins = []string{"*"}
	}
	if !cfg.AuthEnabled || !slices.Contains(origins, "*") {
		return origins
	}

	if defaulted {
		logger.InfoContext(
			context.Background(),
			"cors: auth is enabled and no CORS origins are configured — serving "+
				"same-origin requests only, which is all the built-in web UI needs. "+
				"A browser client served from another origin must be named via "+
				"http.cors_origins / SQI_HTTP_CORS_ORIGINS / --http-cors-origins",
		)
	} else {
		logger.WarnContext(
			context.Background(),
			"cors: auth is enabled but CORS origins include \"*\" — dropping the wildcard, "+
				"because a wildcard origin cannot carry credentials and would let any "+
				"site ride the session cookie. Name the real origins explicitly via "+
				"http.cors_origins / SQI_HTTP_CORS_ORIGINS / --http-cors-origins",
		)
	}
	return slices.DeleteFunc(slices.Clone(origins), func(o string) bool { return o == "*" })
}

// NewRouter builds and returns the chi router that serves the full sqi-server
// HTTP surface. The returned router is ready to be handed to http.Server.
//
// Pass a [Deps] value carrying every application-layer dependency the
// handlers need; optional ones (Hub, Scheduler, Products) may be nil.
func NewRouter(cfg Config, deps Deps, logger *slog.Logger, m *metrics.Metrics, hr *health.Registry) chi.Router {
	origins := resolveCORSOrigins(cfg, logger)

	r := chi.NewRouter()

	// ── Middleware stack ──────────────────────────────────────────────────────

	// 1. Recover from panics before any other middleware runs so that CORS
	//    headers are always present even on 500 responses.
	r.Use(chimiddleware.Recoverer)

	corsOpts := cors.Options{
		AllowedOrigins: origins,
		AllowedMethods: []string{
			"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD",
		},
		AllowedHeaders: []string{
			"Accept",
			"Authorization",
			"Content-Type",
			"X-Request-ID",
		},
		ExposedHeaders: []string{
			"X-Request-ID",
			"X-API-Version",
		},
		// Credentials (the session cookie) are only meaningful once auth is
		// enabled — the browser must be told to send/accept the cookie
		// cross-origin. AllowedOrigins can never contain "*" here: see the
		// wildcard guard above.
		AllowCredentials: cfg.AuthEnabled,
		MaxAge:           300, // seconds to cache preflight response
	}
	if cfg.AuthEnabled && len(origins) == 0 {
		// go-chi/cors treats a nil/empty AllowedOrigins as "allow every
		// origin" by default (see its New()) — the opposite of what an empty
		// explicit allow-list means here (no cross-origin browser access
		// configured). AllowOriginFunc overrides that library default so the
		// wildcard-drop above actually denies cross-origin access instead of
		// silently re-enabling it.
		corsOpts.AllowOriginFunc = func(*http.Request, string) bool { return false }
	}

	// 2. CORS — must run before any handler writes a response so preflight
	//    OPTIONS requests are handled and headers are injected correctly.
	r.Use(cors.Handler(corsOpts))

	// 3. Request ID — chi generates a UUID v4 and writes it into the request
	//    header when X-Request-ID is absent. Our RequestLogger (step 5) then
	//    reads the header, so it always sees a populated ID whether the client
	//    supplied one or chi generated it. Chi's middleware also stores the ID
	//    in the context under chimiddleware.RequestIDKey for chi-ecosystem tools;
	//    our middleware.RequestIDFromContext uses a separate key populated by
	//    RequestLogger — both return the same value.
	r.Use(chimiddleware.RequestID)

	// 4. Gzip compression for clients that send Accept-Encoding: gzip.
	//    Level 5 balances CPU overhead against compression ratio.
	r.Use(chimiddleware.Compress(5))

	// 5. Structured request logging.
	r.Use(middleware.RequestLogger(logger))

	// 6. Prometheus HTTP metrics.
	r.Use(middleware.RequestMetrics(m))

	// ── Observability routes ──────────────────────────────────────────────────

	r.Get("/healthz", hr.LivenessHandler().ServeHTTP)
	r.Get("/readyz", hr.ReadinessHandler().ServeHTTP)
	r.Handle("/metrics", m.Handler())

	if cfg.EnablePprof {
		logger.WarnContext(
			context.Background(),
			"pprof: profiling endpoints enabled — do not expose to untrusted networks",
			slog.String("prefix", "/debug/pprof/"),
		)
		r.HandleFunc("/debug/pprof/", httppprof.Index)
		r.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
		r.HandleFunc("/debug/pprof/profile", httppprof.Profile)
		r.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
		r.HandleFunc("/debug/pprof/trace", httppprof.Trace)
		r.Handle("/debug/pprof/goroutine", httppprof.Handler("goroutine"))
		r.Handle("/debug/pprof/heap", httppprof.Handler("heap"))
		r.Handle("/debug/pprof/allocs", httppprof.Handler("allocs"))
		r.Handle("/debug/pprof/block", httppprof.Handler("block"))
		r.Handle("/debug/pprof/mutex", httppprof.Handler("mutex"))
		r.Handle("/debug/pprof/threadcreate", httppprof.Handler("threadcreate"))
	}

	// ── REST API ────────────────────────────────────────────────
	// Assigned only when non-nil so the handlers' nil checks stay meaningful:
	// a typed-nil *ws.Hub stored in an interface is not itself nil.
	var notifier ws.Notifier
	if deps.Hub != nil {
		notifier = deps.Hub
	}
	// Server-level retry defaults for effective-policy reporting; zero value
	// when no live scheduler is wired (resolution clamps sanely).
	var retryDefaults scheduler.RetryPolicy
	if deps.Scheduler != nil {
		retryDefaults = deps.Scheduler.RetryDefaults()
	}
	jobs := newJobHandler(deps.Store, deps.Submitter, deps.Scheduler, notifier, logger, retryDefaults, cfg.ValidateJobOwner)
	tasks := newTaskHandler(deps.Store, deps.Scheduler, logger)
	workers := newWorkerHandler(deps.Store, notifier, cfg.WorkerOfflineThreshold, logger)
	farms := newFarmHandler(deps.Store, logger)
	queues := newQueueHandler(deps.Store, logger)
	storageLocs := newStorageLocationHandler(deps.Store, logger)
	computeLocs := newComputeLocationHandler(deps.Store, logger)
	usagePools := newUsagePoolHandler(deps.Store, logger)
	products := newProductHandler(deps.Products, deps.Submitter, deps.Scheduler, deps.Store, logger, cfg.ValidateJobOwner)
	presets := newPresetHandler(deps.PresetLib, deps.Products, deps.Store, logger)
	diagnostics := newDiagnosticsHandler(deps.DiagReader, logger)
	versionH := newVersionHandler(deps.Version)
	authH := newAuthHandler(authHandlerDeps{
		Store:        deps.Store,
		Logger:       logger,
		TTL:          deps.SessionTTL,
		CookieName:   deps.CookieName,
		CookieSecure: deps.CookieSecure,
		LDAPVerifier: deps.LDAPVerifier,
		LDAPConfig:   deps.LDAPConfig,
		OIDCProvider: deps.OIDCProvider,
		OIDCConfig:   deps.OIDCConfig,
		OIDCStateKey: deps.OIDCStateKey,
	})
	if cfg.AuthEnabled {
		// Pay the argon2id derivation here, not on the first login that misses
		// a username — see dummyHash.
		warmDummyHash()
	}
	usersH := newUsersHandler(deps.Store, logger, deps.LDAPConfig.RoleSource)
	apiKeysH := newAPIKeysHandler(deps.Store, logger)
	az := newAuthz(deps.Store, logger)

	wsH := newWSHandler(logger, deps.Hub, deps.Store, deps.Auth, wsOriginConfig{
		Enabled:        cfg.AuthEnabled,
		AllowedOrigins: origins,
	})

	r.Route("/api/v1", func(api chi.Router) {
		// 7. Versioning header — X-API-Version: 1 on every response.
		api.Use(middleware.APIVersion("1"))

		// 8. Per-IP token-bucket rate limiting (applies to REST + /ws).
		//    20 req/s sustained, burst of 40. Still per-IP rather than
		//    per-principal; auth-aware limits remain a possible refinement.
		//    Disabled via cfg.DisableRateLimit (tests/benchmarks).
		if !cfg.DisableRateLimit {
			api.Use(middleware.RateLimit(middleware.RateLimitConfig{
				RequestsPerSecond: 20,
				Burst:             40,
			}, logger))
		}

		// WebSocket upgrade — gated by its own hook (WS token transport quirk),
		// so it stays outside the middleware.Auth group.
		api.Get("/ws", wsH.ServeHTTP)

		// OpenAPI spec — deliberately public: it documents the auth scheme
		// itself, so requiring auth to read it would be circular.
		api.Get("/openapi.yaml", serveOpenAPISpec)

		// Public: login mints the session cookie (no principal required yet,
		// so it cannot be gated by middleware.Auth).
		api.Post("/auth/login", authH.login)

		// Also public, for the same reason, and only mounted when SSO is
		// configured: an unconfigured deployment should 404 rather than
		// advertise a route that cannot work. Both are browser navigations
		// that redirect; see oidclogin.go for why they are GETs that
		// middleware.CSRF does not (and cannot) cover.
		if deps.OIDCProvider != nil {
			api.Get("/auth/oidc/login", authH.oidcLogin)
			api.Get("/auth/oidc/callback", authH.oidcCallback)
		}

		// REST resource routes — gated by the auth middleware.
		api.Group(func(rest chi.Router) {
			// CSRF must run before Auth: it only cares about the presence of
			// the session cookie (ambient credential), not about whether
			// authentication itself succeeds. Mounted only when auth is
			// enabled — with no session cookie in play there is nothing to
			// guard, and mounting it with an empty CookieName would compare
			// against a cookie that never exists (a no-op in practice, but
			// pointless overhead on every request).
			if cfg.AuthEnabled {
				rest.Use(middleware.CSRF(middleware.CSRFConfig{
					CookieName:     deps.CookieName,
					AllowedOrigins: origins,
				}, logger))
			}
			rest.Use(middleware.Auth(deps.Auth, logger))

			// Permission-free (any authenticated principal).
			rest.Post("/auth/logout", authH.logout)
			rest.Get("/auth/me", authH.me)
			rest.Patch("/auth/me", authH.updateMe)
			rest.Put("/auth/password", authH.changePassword)
			rest.Get("/version", versionH.getVersion)

			// users.read / users.manage (admin only)
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.UsersRead))
				g.Get("/users", usersH.list)
				g.Get("/users/{id}", usersH.get)
			})
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.UsersManage))
				g.Post("/users", usersH.create)
				g.Patch("/users/{id}", usersH.update)
				g.Put("/users/{id}/password", usersH.setPassword)
				g.Delete("/users/{id}", usersH.delete)
			})

			// apikeys.self (all authenticated roles)
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.APIKeysSelf))
				g.Post("/api-keys", apiKeysH.create)
				g.Get("/api-keys", apiKeysH.list)
				g.Delete("/api-keys/{id}", apiKeysH.revoke)
			})

			// apikeys.admin (admin only) — anyone's keys, list and revoke only.
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.APIKeysAdmin))
				g.Get("/users/{id}/api-keys", apiKeysH.listForUser)
				g.Delete("/users/{id}/api-keys/{keyId}", apiKeysH.revokeForUser)
			})

			// jobs.read
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.JobsRead))
				// Collection route: scoped inside the handler via scopeFilter.
				g.Get("/jobs", jobs.listJobs)
				// Object routes: owner-enforced. The middleware is chosen by what
				// {id} names on each route — a job id or a task id.
				g.With(az.requireJobAccessByJobID()).Get("/jobs/{id}", jobs.getJob)
				g.With(az.requireJobAccessByJobID()).Get("/jobs/{id}/tasks", tasks.listJobTasks)
				g.With(az.requireJobAccessByTaskID()).Get("/tasks/{id}", tasks.getTask)
				g.With(az.requireJobAccessByTaskID()).Get("/tasks/{id}/logs", tasks.getTaskLogs)
				g.With(az.requireJobAccessByTaskID()).Get("/tasks/{id}/attempts", tasks.getTaskAttempts)
			})
			// jobs.write
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.JobsWrite))
				// Creation routes: no prior object; identity is bound in the
				// handler (see submitidentity.go).
				g.Post("/jobs", jobs.submitJob)
				g.Post("/products/{name}/jobs", products.submitProductJob)
				// Object routes: owner-enforced. This is what closes B1's
				// carried-forward gap, where a `user` could cancel any job.
				g.With(az.requireJobAccessByJobID()).Patch("/jobs/{id}", jobs.patchJob)
				g.With(az.requireJobAccessByJobID()).Post("/jobs/{id}/cancel", jobs.cancelJob)
				g.With(az.requireJobAccessByJobID()).Post("/jobs/{id}/retry", jobs.retryJob)
				g.With(az.requireJobAccessByJobID()).Delete("/jobs/{id}", jobs.deleteJob)
				g.With(az.requireJobAccessByTaskID()).Post("/tasks/{id}/retry", tasks.retryTask)
				g.With(az.requireJobAccessByTaskID()).Post("/tasks/{id}/cancel", tasks.cancelTask)
			})

			// workers.read / workers.manage
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.WorkersRead))
				g.Get("/workers", workers.listWorkers)
				g.Get("/workers/{id}", workers.getWorker)
			})
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.WorkersManage))
				g.Post("/workers/{id}/disable", workers.disableWorker)
				g.Post("/workers/{id}/enable", workers.enableWorker)
				g.Delete("/workers/{id}", workers.removeWorker)
			})

			// infra.read / infra.manage (farms, queues, storage, compute, usage-pools)
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.InfraRead))
				g.Get("/farms", farms.listFarms)
				g.Get("/farms/{id}", farms.getFarm)
				g.Get("/queues", queues.listQueues)
				g.Get("/queues/{id}", queues.getQueue)
				g.Get("/storage-locations", storageLocs.listStorageLocations)
				g.Get("/storage-locations/{id}", storageLocs.getStorageLocation)
				g.Get("/compute-locations", computeLocs.listComputeLocations)
				g.Get("/compute-locations/{id}", computeLocs.getComputeLocation)
				g.Get("/usage-pools", usagePools.listUsagePools)
				g.Get("/usage-pools/{id}", usagePools.getUsagePool)
			})
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.InfraManage))
				g.Post("/farms", farms.createFarm)
				g.Put("/farms/{id}", farms.updateFarm)
				g.Delete("/farms/{id}", farms.deleteFarm)
				g.Post("/queues", queues.createQueue)
				g.Put("/queues/{id}", queues.updateQueue)
				g.Delete("/queues/{id}", queues.deleteQueue)
				g.Post("/storage-locations", storageLocs.createStorageLocation)
				g.Put("/storage-locations/{id}", storageLocs.updateStorageLocation)
				g.Delete("/storage-locations/{id}", storageLocs.deleteStorageLocation)
				g.Post("/compute-locations", computeLocs.createComputeLocation)
				g.Put("/compute-locations/{id}", computeLocs.updateComputeLocation)
				g.Delete("/compute-locations/{id}", computeLocs.deleteComputeLocation)
				g.Post("/usage-pools", usagePools.createUsagePool)
				g.Put("/usage-pools/{id}", usagePools.updateUsagePool)
				g.Delete("/usage-pools/{id}", usagePools.deleteUsagePool)
			})

			// products.read / products.manage
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.ProductsRead))
				g.Get("/products", products.listProducts)
				g.Get("/products/{name}", products.getProduct)
				g.Get("/products/{name}/parameters", products.getProductParameters)
				g.Get("/presets", presets.listPresets)
				g.Get("/presets/{name}", presets.getPreset)
			})
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.ProductsManage))
				g.Post("/products", products.createProduct)
				g.Put("/products/{name}", products.updateProduct)
				g.Delete("/products/{name}", products.deleteProduct)
				g.Post("/presets/{name}/install", presets.installPreset)
			})

			// diagnostics.read
			rest.Group(func(g chi.Router) {
				g.Use(az.require(policy.DiagnosticsRead))
				g.Get("/diagnostics/logs", diagnostics.getDiagnosticsLogs)
			})
		})
	})

	// ── Embedded web UI ────────────────────────────────────────
	// Mounted as the root catch-all, so it only handles paths the API and
	// observability routes above did not claim. The handler serves embedded
	// assets and falls back to the SPA shell for client-side routes under /ui/*.
	if uiHandler, err := ui.NewHandler(logger); err != nil {
		// A failure here means the embedded asset tree is malformed — a
		// build-time problem. Degrade gracefully: log and leave the UI
		// unmounted so the REST API and WebSocket remain fully functional.
		logger.ErrorContext(
			context.Background(),
			"ui: embedded assets unavailable, web UI disabled",
			slog.Any("error", err),
		)
	} else {
		r.Handle("/*", uiHandler)
	}

	return r
}
