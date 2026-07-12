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
	httppprof "net/http/pprof"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

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
}

// NewRouter builds and returns the chi router that serves the full sqi-server
// HTTP surface. The returned router is ready to be handed to http.Server.
//
// Route groups for the REST API (/api/v1/*) and WebSocket (/api/v1/ws) are
// populated incrementally as each task is completed. Pass a [Deps] value with
// all application-layer dependencies for the currently implemented handlers.
func NewRouter(cfg Config, deps Deps, logger *slog.Logger, m *metrics.Metrics, hr *health.Registry) chi.Router {
	origins := cfg.CORSOrigins
	if len(origins) == 0 {
		origins = []string{"*"}
	}

	r := chi.NewRouter()

	// ── Middleware stack ──────────────────────────────────────────────────────

	// 1. Recover from panics before any other middleware runs so that CORS
	//    headers are always present even on 500 responses.
	r.Use(chimiddleware.Recoverer)

	// 2. CORS — must run before any handler writes a response so preflight
	//    OPTIONS requests are handled and headers are injected correctly.
	r.Use(cors.Handler(cors.Options{
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
		// Credentials (cookies / Authorization header) are only relevant once
		// auth is introduced in Phase 3. Set to false until then.
		AllowCredentials: false,
		MaxAge:           300, // seconds to cache preflight response
	}))

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
	var jobNotifier ws.Notifier
	if deps.Hub != nil {
		jobNotifier = deps.Hub
	}
	jobs := newJobHandler(deps.Store, deps.Submitter, deps.Scheduler, jobNotifier, logger)
	tasks := newTaskHandler(deps.Store, deps.Scheduler, logger)
	// Pass the hub as a notifier only when non-nil so the worker handler's
	// nil check is meaningful (a typed-nil *ws.Hub in an interface is not nil).
	var workerNotifier ws.Notifier
	if deps.Hub != nil {
		workerNotifier = deps.Hub
	}
	workers := newWorkerHandler(deps.Store, workerNotifier, cfg.WorkerOfflineThreshold, logger)
	farms := newFarmHandler(deps.Store, logger)
	queues := newQueueHandler(deps.Store, logger)
	storageLocs := newStorageLocationHandler(deps.Store, logger)
	computeLocs := newComputeLocationHandler(deps.Store, logger)
	usagePools := newUsagePoolHandler(deps.Store, logger)
	products := newProductHandler(deps.Products, deps.Submitter, deps.Scheduler, logger)
	presets := newPresetHandler(deps.PresetLib, deps.Products, deps.Store, logger)
	diagnostics := newDiagnosticsHandler(deps.DiagReader, logger)
	versionH := newVersionHandler(deps.Version)

	r.Route("/api/v1", func(api chi.Router) {
		// 7. Versioning header — X-API-Version: 1 on every response.
		api.Use(middleware.APIVersion("1"))

		// 8. Per-IP token-bucket rate limiting (Phase 1; no auth yet).
		//    20 req/s sustained, burst of 40.  Replace with auth-aware
		//    per-client limits when Phase 3 auth lands.
		//    Disabled via cfg.DisableRateLimit (tests/benchmarks).
		if !cfg.DisableRateLimit {
			api.Use(middleware.RateLimit(middleware.RateLimitConfig{
				RequestsPerSecond: 20,
				Burst:             40,
			}, logger))
		}

		// ── Job endpoints ───────────────────────────────────
		api.Post("/jobs", jobs.submitJob)
		api.Get("/jobs", jobs.listJobs)
		api.Get("/jobs/{id}", jobs.getJob)
		api.Patch("/jobs/{id}", jobs.patchJob)
		api.Post("/jobs/{id}/cancel", jobs.cancelJob)
		api.Post("/jobs/{id}/retry", jobs.retryJob)
		api.Delete("/jobs/{id}", jobs.deleteJob)

		// ── Task endpoints ──────────────────────────────────
		api.Get("/jobs/{id}/tasks", tasks.listJobTasks)
		api.Get("/tasks/{id}", tasks.getTask)
		api.Get("/tasks/{id}/logs", tasks.getTaskLogs)
		api.Get("/tasks/{id}/attempts", tasks.getTaskAttempts)
		api.Post("/tasks/{id}/retry", tasks.retryTask)
		api.Post("/tasks/{id}/cancel", tasks.cancelTask)

		// ── Worker endpoints ───────────────────────────────
		api.Get("/workers", workers.listWorkers)
		api.Get("/workers/{id}", workers.getWorker)
		api.Post("/workers/{id}/disable", workers.disableWorker)
		api.Post("/workers/{id}/enable", workers.enableWorker)
		api.Delete("/workers/{id}", workers.removeWorker)

		// ── Farm endpoints ──────────────────────────────────────
		api.Post("/farms", farms.createFarm)
		api.Get("/farms", farms.listFarms)
		api.Get("/farms/{id}", farms.getFarm)
		api.Put("/farms/{id}", farms.updateFarm)
		api.Delete("/farms/{id}", farms.deleteFarm)

		// ── Queue endpoints ─────────────────────────────────────
		api.Post("/queues", queues.createQueue)
		api.Get("/queues", queues.listQueues)
		api.Get("/queues/{id}", queues.getQueue)
		api.Put("/queues/{id}", queues.updateQueue)
		api.Delete("/queues/{id}", queues.deleteQueue)

		// ── Storage-location endpoints ──────────────────────────
		api.Post("/storage-locations", storageLocs.createStorageLocation)
		api.Get("/storage-locations", storageLocs.listStorageLocations)
		api.Get("/storage-locations/{id}", storageLocs.getStorageLocation)
		api.Put("/storage-locations/{id}", storageLocs.updateStorageLocation)
		api.Delete("/storage-locations/{id}", storageLocs.deleteStorageLocation)

		// ── Compute-location endpoints ──────────────────────────
		api.Post("/compute-locations", computeLocs.createComputeLocation)
		api.Get("/compute-locations", computeLocs.listComputeLocations)
		api.Get("/compute-locations/{id}", computeLocs.getComputeLocation)
		api.Put("/compute-locations/{id}", computeLocs.updateComputeLocation)
		api.Delete("/compute-locations/{id}", computeLocs.deleteComputeLocation)

		// ── Product endpoints ───────────────────────────────────
		api.Get("/products", products.listProducts)
		api.Post("/products", products.createProduct)
		api.Get("/products/{name}", products.getProduct)
		api.Put("/products/{name}", products.updateProduct)
		api.Delete("/products/{name}", products.deleteProduct)
		api.Post("/products/{name}/jobs", products.submitProductJob)
		api.Get("/products/{name}/parameters", products.getProductParameters)

		// ── Preset endpoints ────────────────────────────────────
		api.Get("/presets", presets.listPresets)
		api.Get("/presets/{name}", presets.getPreset)
		api.Post("/presets/{name}/install", presets.installPreset)

		// ── Usage-pool endpoints ────────────────────────────────
		api.Post("/usage-pools", usagePools.createUsagePool)
		api.Get("/usage-pools", usagePools.listUsagePools)
		api.Get("/usage-pools/{id}", usagePools.getUsagePool)
		api.Put("/usage-pools/{id}", usagePools.updateUsagePool)
		api.Delete("/usage-pools/{id}", usagePools.deleteUsagePool)

		// ── Diagnostics endpoints ───────────────────────────────
		api.Get("/diagnostics/logs", diagnostics.getDiagnosticsLogs)

		// ── Version endpoint ────────────────────────────────────
		api.Get("/version", versionH.getVersion)

		// OpenAPI spec.
		api.Get("/openapi.yaml", serveOpenAPISpec)

		// WebSocket upgrade.
		wsH := newWSHandler(logger, deps.Hub)
		api.Get("/ws", wsH.ServeHTTP)
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
