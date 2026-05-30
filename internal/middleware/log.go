// SPDX-License-Identifier: AGPL-3.0-only

// Package middleware provides net/http middleware for sqi-server.
//
// Middleware in this package is router-agnostic: each function returns a
// standard func(http.Handler) http.Handler that can be composed directly or
// mounted via any router (chi, gorilla/mux, net/http ServeMux, etc.).
package middleware

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ctxKey is an unexported type for context keys owned by this package.
// Using a named type prevents collisions with keys from other packages.
type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDFromContext returns the request ID attached to ctx by
// [RequestLogger], or an empty string if ctx carries no request ID.
func RequestIDFromContext(ctx context.Context) string {
	id, ok := ctx.Value(requestIDKey).(string)
	if !ok {
		return ""
	}
	return id
}

// RequestLogger returns middleware that, for every request:
//
//  1. Reads the X-Request-ID header; if absent, generates a random 16-char
//     hex ID using crypto/rand.
//  2. Stores the request ID in the request context (retrieve with
//     [RequestIDFromContext]).
//  3. Echoes the request ID in the X-Request-ID response header.
//  4. Wraps the [http.ResponseWriter] to capture the status code.
//  5. After the handler returns, logs method, path, status code, duration,
//     and request ID at Info level using the provided [*slog.Logger].
//
// WebSocket upgrades (HTTP 101) are handled transparently: the wrapped writer
// implements [http.Hijacker] so the upgrade handshake proceeds normally, and
// the middleware logs the 101 status after the connection is hijacked.
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// ── Request ID ────────────────────────────────────────────────
			id := r.Header.Get("X-Request-ID")
			if id == "" {
				id = generateID()
			}

			// Store in context so handlers can include it in their own logs.
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			r = r.WithContext(ctx)

			// Echo back in response regardless of whether we generated it.
			w.Header().Set("X-Request-ID", id)

			// ── Wrap writer to capture status code ────────────────────────
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

			// ── Delegate ──────────────────────────────────────────────────
			next.ServeHTTP(rw, r)

			// ── Log ───────────────────────────────────────────────────────
			logger.InfoContext(
				ctx, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rw.status),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", id),
			)
		})
	}
}

// generateID returns a cryptographically random 16-character lowercase hex
// string (8 random bytes). It panics only if crypto/rand is broken, which is
// treated as an unrecoverable environment failure.
func generateID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("middleware: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// responseWriter wraps [http.ResponseWriter] to intercept WriteHeader and
// capture the status code for logging. It also delegates [http.Flusher] and
// [http.Hijacker] to the underlying writer so streaming responses and
// WebSocket upgrades continue to work.
type responseWriter struct {
	http.ResponseWriter

	status  int
	written bool
}

// WriteHeader captures the status code and forwards it to the underlying
// writer. If WriteHeader has already been called for this request the call is
// ignored (matching net/http semantics).
func (rw *responseWriter) WriteHeader(code int) {
	if rw.written {
		return
	}
	rw.status = code
	rw.written = true
	rw.ResponseWriter.WriteHeader(code)
}

// Write forwards the body to the underlying writer. If WriteHeader has not
// been called yet it is implicitly called with 200, matching net/http
// semantics.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// Flush implements [http.Flusher] by delegating to the underlying writer if it
// supports flushing. Required for server-sent events and chunked streaming.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements [http.Hijacker] by delegating to the underlying writer if
// it supports hijacking. Required for WebSocket upgrades.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		// Once hijacked the status code is no longer meaningful, but mark it
		// so the log line records 101 rather than the 200 default.
		if !rw.written {
			rw.status = http.StatusSwitchingProtocols
			rw.written = true
		}
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
