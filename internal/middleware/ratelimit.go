// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"sync"

	"golang.org/x/time/rate"
)

// RateLimitConfig controls the per-IP token-bucket parameters.
type RateLimitConfig struct {
	// RequestsPerSecond is the steady-state refill rate for each IP bucket.
	// Defaults to 20 when zero.
	RequestsPerSecond rate.Limit

	// Burst is the maximum number of requests allowed in a single instant.
	// Defaults to 40 when zero.
	Burst int
}

// RateLimit returns middleware that enforces a per-IP token-bucket rate limit.
// Clients that exceed the limit receive HTTP 429 Too Many Requests.
//
// Each remote IP maintains its own bucket; buckets are allocated lazily on
// first request.
//
// Phase 1 limitations (both deferred to Phase 3):
//
//   - No LRU eviction: the limiter map grows unbounded. This is acceptable for
//     Phase 1's trusted local deployments where the client IP space is small.
//     Phase 3 will introduce an LRU cache (e.g. golang.org/x/time/rate +
//     github.com/hashicorp/golang-lru) sized to expected concurrent client count.
//
//   - RemoteAddr only: when a reverse proxy is in front, all clients share the
//     proxy's IP and effectively one bucket. Phase 3 will honor
//     X-Forwarded-For / X-Real-IP after the trusted-proxy list is configured
//     alongside the auth layer.
//
// Usage (applied to the /api/v1 sub-router):
//
//	api.Use(middleware.RateLimit(middleware.RateLimitConfig{
//	    RequestsPerSecond: 20,
//	    Burst:             40,
//	}, logger))
func RateLimit(cfg RateLimitConfig, logger *slog.Logger) func(http.Handler) http.Handler {
	if cfg.RequestsPerSecond <= 0 {
		cfg.RequestsPerSecond = 20
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 40
	}

	var mu sync.Mutex
	limiters := make(map[string]*rate.Limiter)

	getLimiter := func(ip string) *rate.Limiter {
		mu.Lock()
		defer mu.Unlock()
		l, ok := limiters[ip]
		if !ok {
			l = rate.NewLimiter(cfg.RequestsPerSecond, cfg.Burst)
			limiters[ip] = l
		}
		return l
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				// Malformed RemoteAddr — fall back to the raw value.
				ip = r.RemoteAddr
			}

			if !getLimiter(ip).Allow() {
				logger.DebugContext(
					r.Context(), "middleware: rate limit exceeded",
					slog.String("remote_ip", ip),
					slog.String("path", r.URL.Path),
				)
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
