// SPDX-License-Identifier: AGPL-3.0-only

// Package api wires together the sqi-server HTTP surface: the chi router,
// standard middleware stack, and all route groups.
//
// # Router layout
//
//	GET  /healthz               — liveness probe  (task 23)
//	GET  /readyz                — readiness probe  (task 23)
//	GET  /metrics               — Prometheus scrape endpoint (task 22)
//	GET  /debug/pprof/*         — Go runtime profiling (task 24, gated by config)
//	/api/v1/*                   — REST API (tasks 71–86)
//	/api/v1/ws                  — WebSocket upgrade (tasks 87–92)
//	GET  /api/v1/openapi.yaml   — served OpenAPI spec (task 83)
//	/*                          — embedded SPA + static assets (tasks 93–95)
//
// # Middleware stack (outermost → innermost)
//
//  1. chi Recoverer          — catches panics, returns 500, logs stack trace
//  2. go-chi/cors            — CORS preflight + headers (origins from Config)
//  3. chi RequestID          — sets X-Request-ID; chi's version generates a
//     UUID when the header is absent
//  4. chi Compress           — gzip response bodies for clients that accept it
//  5. middleware.RequestLogger  — structured slog request logging (task 21)
//  6. middleware.RequestMetrics — Prometheus HTTP metrics (task 22)
//
// Note: the structured-logging and metrics middleware from task 21/22 are
// mounted here instead of the now-removed obsServer so every HTTP and
// WebSocket request continues to be logged and counted.
package api

import (
	"context"
	"log/slog"
	httppprof "net/http/pprof"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/middleware"
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
}

// NewRouter builds and returns the chi router that serves the full sqi-server
// HTTP surface. The returned router is ready to be handed to http.Server.
//
// Route groups for the REST API (/api/v1/*) and WebSocket (/api/v1/ws) are
// registered as stub mounts initially and filled in by tasks 71–92.
func NewRouter(cfg Config, logger *slog.Logger, m *metrics.Metrics, hr *health.Registry) chi.Router {
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

	// 5. Structured request logging (task 21).
	r.Use(middleware.RequestLogger(logger))

	// 6. Prometheus HTTP metrics (task 22).
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

	// ── REST API (tasks 71–86) ────────────────────────────────────────────────
	// Sub-router is mounted now so the middleware chain is in place. Route
	// registrations are added task-by-task in subsequent PRs.
	r.Route("/api/v1", func(_ chi.Router) {
		// TODO(task 71): POST /api/v1/jobs
		// TODO(task 72): GET  /api/v1/jobs
		// TODO(task 73): GET  /api/v1/jobs/{id}
		// TODO(task 74): PATCH /api/v1/jobs/{id}
		// TODO(task 75): DELETE /api/v1/jobs/{id}
		// TODO(task 76): GET  /api/v1/jobs/{id}/tasks, GET /api/v1/tasks/{id}
		// TODO(task 77): GET  /api/v1/tasks/{id}/logs
		// TODO(task 78): POST /api/v1/tasks/{id}/retry
		// TODO(task 79): GET  /api/v1/workers, GET /api/v1/workers/{id}
		// TODO(task 80): POST /api/v1/workers/{id}/disable|enable
		// TODO(task 81): CRUD /api/v1/farms|queues|storage-locations|license-pools
		// TODO(task 83): GET  /api/v1/openapi.yaml
		// TODO(task 87): /api/v1/ws — WebSocket upgrade
	})

	// ── Embedded web UI (tasks 93–95) ────────────────────────────────────────
	// TODO(task 93): serve embedded web/dist at "/" with SPA fallback.

	return r
}
