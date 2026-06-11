// SPDX-License-Identifier: AGPL-3.0-or-later

// Package obs provides the local observability HTTP server for sqi-worker.
//
// The server is intentionally minimal and listens on a loopback address
// (default 127.0.0.1:9091) so it is accessible to local monitoring agents
// and container orchestrators without being reachable from the network.
//
// # Endpoints
//
//   - GET /healthz  — liveness probe; always 200 while the HTTP event loop runs.
//   - GET /readyz   — readiness probe; 200 when all registered [health.Checker]s
//     pass, 503 when any fail (e.g. NATS connection is down).
//   - GET /metrics  — Prometheus text/OpenMetrics exposition.
//   - GET /debug/pprof/* — Go runtime profiling (only when pprof is enabled).
//
// # Usage
//
//	srv := obs.New(cfg, logger, metrics, health)
//	go srv.Run(ctx)      // starts listening; returns when ctx is canceled
//	// ... after work is done ...
//	srv.Shutdown(ctx)    // graceful stop
package obs

import (
	"context"
	"log/slog"
	"net/http"
	httppprof "net/http/pprof"
	"time"

	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/middleware"
	workmetrics "github.com/uberware/sqi/internal/worker/metrics"
)

const (
	// readHeaderTimeout is the maximum time the server waits to read request
	// headers. Guards against Slowloris-style attacks on the loopback interface.
	readHeaderTimeout = 10 * time.Second

	// uptimeTick is how often UptimeSeconds is refreshed.
	uptimeTick = 15 * time.Second
)

// Server is the local observability HTTP server for sqi-worker.
// Create with [New]; start with [Run]; stop with [Shutdown].
type Server struct {
	addr        string
	enablePprof bool
	logger      *slog.Logger
	metrics     *workmetrics.Metrics
	health      *health.Registry
	httpServer  *http.Server
}

// New creates a [*Server] with the given configuration.
//
//   - addr        — TCP listen address, e.g. "127.0.0.1:9091"
//   - enablePprof — when true, registers /debug/pprof/* endpoints
//   - logger      — used for request logging and startup/shutdown messages
//   - m           — worker Prometheus metrics; /metrics serves this registry
//   - h           — health registry consulted by /readyz
func New(
	addr string,
	enablePprof bool,
	logger *slog.Logger,
	m *workmetrics.Metrics,
	h *health.Registry,
) *Server {
	return &Server{
		addr:        addr,
		enablePprof: enablePprof,
		logger:      logger,
		metrics:     m,
		health:      h,
	}
}

// Run starts the HTTP server and blocks until ctx is canceled, at which point
// it performs a graceful shutdown and returns.
//
// Run also starts a background goroutine that refreshes the uptime metric on
// [uptimeTick] intervals; that goroutine exits when ctx is canceled.
func (s *Server) Run(ctx context.Context) {
	mux := s.buildMux(ctx)

	// Apply middleware: request logger wraps the whole mux.
	var handler http.Handler = mux
	handler = middleware.RequestLogger(s.logger)(handler)

	s.httpServer = &http.Server{
		Addr:              s.addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Uptime ticker — keep the gauge fresh while the server runs.
	go func() {
		t := time.NewTicker(uptimeTick)
		defer t.Stop()
		s.metrics.UpdateUptime()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.metrics.UpdateUptime()
			}
		}
	}()

	s.logger.InfoContext(
		ctx, "obs: http server starting",
		slog.String("addr", s.addr),
		slog.Bool("pprof", s.enablePprof),
	)

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.logger.ErrorContext(ctx, "obs: http server error", slog.Any("error", err))
	}
}

// Shutdown gracefully stops the HTTP server within the deadline of the
// supplied context.
func (s *Server) Shutdown(ctx context.Context) {
	if s.httpServer == nil {
		return
	}
	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.logger.ErrorContext(ctx, "obs: shutdown error", slog.Any("error", err))
	}
}

// buildMux constructs the HTTP mux with all observability routes registered.
func (s *Server) buildMux(ctx context.Context) *http.ServeMux {
	mux := http.NewServeMux()

	// ── Health probes ─────────────────────────────────────────────────────
	mux.Handle("/healthz", s.health.LivenessHandler())
	mux.Handle("/readyz", s.health.ReadinessHandler())

	// ── Prometheus metrics ────────────────────────────────────────────────
	mux.Handle("/metrics", s.metrics.Handler())

	// ── pprof (optional) ─────────────────────────────────────────────────
	if s.enablePprof {
		s.logger.WarnContext(
			ctx,
			"obs: pprof endpoints enabled — do not expose to untrusted networks",
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

	return mux
}
