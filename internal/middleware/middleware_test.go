// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware_test

// Tests for RequestLogger, APIVersion, Deprecated, and RateLimit middleware.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/middleware"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// okHandler is a simple 200 OK handler used as the inner handler in tests.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func newReq(method, target string) *http.Request {
	return httptest.NewRequestWithContext(context.Background(), method, target, nil)
}

// ── RequestLogger ─────────────────────────────────────────────────────────────

func TestRequestLogger_SetsRequestIDHeader(t *testing.T) {
	h := middleware.RequestLogger(discardLogger())(okHandler)
	req := newReq(http.MethodGet, "/test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	id := rr.Header().Get("X-Request-ID")
	if id == "" {
		t.Error("X-Request-ID header not set")
	}
}

func TestRequestLogger_EchosProvidedRequestID(t *testing.T) {
	h := middleware.RequestLogger(discardLogger())(okHandler)
	req := newReq(http.MethodGet, "/test")
	req.Header.Set("X-Request-ID", "my-custom-id")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Header().Get("X-Request-ID") != "my-custom-id" {
		t.Errorf("echoed id = %q, want my-custom-id", rr.Header().Get("X-Request-ID"))
	}
}

func TestRequestLogger_RequestIDAvailableInContext(t *testing.T) {
	var ctxID string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctxID = middleware.RequestIDFromContext(r.Context())
	})

	h := middleware.RequestLogger(discardLogger())(inner)
	req := newReq(http.MethodGet, "/test")
	req.Header.Set("X-Request-ID", "ctx-check")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if ctxID != "ctx-check" {
		t.Errorf("ctx id = %q, want ctx-check", ctxID)
	}
}

func TestRequestIDFromContext_Missing(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	id := middleware.RequestIDFromContext(req.Context())
	if id != "" {
		t.Errorf("expected empty string for context without id, got %q", id)
	}
}

// ── APIVersion ────────────────────────────────────────────────────────────────

func TestAPIVersion_SetsHeader(t *testing.T) {
	h := middleware.APIVersion("1")(okHandler)
	req := newReq(http.MethodGet, "/api/v1/something")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if v := rr.Header().Get("X-API-Version"); v != "1" {
		t.Errorf("X-API-Version = %q, want 1", v)
	}
}

func TestAPIVersion_HeaderPresentOnError(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := middleware.APIVersion("2")(inner)
	req := newReq(http.MethodGet, "/test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if v := rr.Header().Get("X-API-Version"); v != "2" {
		t.Errorf("X-API-Version missing on error response: %q", v)
	}
}

// ── Deprecated ────────────────────────────────────────────────────────────────

func TestDeprecated_WithDate(t *testing.T) {
	declared := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sunset := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	h := middleware.Deprecated(declared, sunset, "https://docs.example.com/deprecation")(okHandler)

	req := newReq(http.MethodGet, "/old")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if dep := rr.Header().Get("Deprecation"); dep == "" {
		t.Error("Deprecation header not set")
	}
	if sun := rr.Header().Get("Sunset"); sun == "" {
		t.Error("Sunset header not set")
	}
	link := rr.Header().Get("Link")
	if !strings.Contains(link, "rel=\"deprecation\"") {
		t.Errorf("Link header missing rel=deprecation: %q", link)
	}
}

func TestDeprecated_ZeroDeclaredAt(t *testing.T) {
	h := middleware.Deprecated(time.Time{}, time.Time{}, "")(okHandler)
	req := newReq(http.MethodGet, "/old")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	dep := rr.Header().Get("Deprecation")
	if dep != "true" {
		t.Errorf("Deprecation = %q, want \"true\" for zero time", dep)
	}
	if sun := rr.Header().Get("Sunset"); sun != "" {
		t.Errorf("Sunset should be absent for zero sunsetDate, got %q", sun)
	}
	if link := rr.Header().Get("Link"); link != "" {
		t.Errorf("Link should be absent for empty link, got %q", link)
	}
}

// ── RateLimit ─────────────────────────────────────────────────────────────────

func TestRateLimit_AllowsWithinBurst(t *testing.T) {
	cfg := middleware.RateLimitConfig{RequestsPerSecond: 100, Burst: 10}
	h := middleware.RateLimit(cfg, discardLogger())(okHandler)

	for i := range 5 {
		req := newReq(http.MethodGet, "/api")
		req.RemoteAddr = "127.0.0.1:12345"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i, rr.Code)
		}
	}
}

func TestRateLimit_RejectsWhenExceeded(t *testing.T) {
	// Very tight limit: 0.001 rps, burst 1.
	cfg := middleware.RateLimitConfig{RequestsPerSecond: 0.001, Burst: 1}
	h := middleware.RateLimit(cfg, discardLogger())(okHandler)

	// First request consumes the burst.
	req1 := newReq(http.MethodGet, "/api")
	req1.RemoteAddr = "10.0.0.1:9999"
	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, req1)

	// Second request immediately after should be rejected.
	req2 := newReq(http.MethodGet, "/api")
	req2.RemoteAddr = "10.0.0.1:9999"
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 when limit exceeded, got %d", rr2.Code)
	}
}

func TestRateLimit_RejectionIsProblemJSONWithRetryAfter(t *testing.T) {
	// Tight limit: the second request is rejected and must carry the standard
	// RFC 7807 error body plus a delta-seconds Retry-After header, matching
	// the error conventions in docs/api.md.
	cfg := middleware.RateLimitConfig{RequestsPerSecond: 0.5, Burst: 1}
	h := middleware.RateLimit(cfg, discardLogger())(okHandler)

	req1 := newReq(http.MethodGet, "/api")
	req1.RemoteAddr = "10.0.0.2:9999"
	h.ServeHTTP(httptest.NewRecorder(), req1)

	req2 := newReq(http.MethodGet, "/api")
	req2.RemoteAddr = "10.0.0.2:9999"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req2)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("want application/problem+json, got %q", ct)
	}

	retryAfter := rr.Header().Get("Retry-After")
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil {
		t.Fatalf("Retry-After %q is not delta-seconds: %v", retryAfter, err)
	}
	if seconds < 1 {
		t.Fatalf("want Retry-After >= 1, got %d", seconds)
	}

	var body struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Status != http.StatusTooManyRequests {
		t.Errorf("want body status 429, got %d", body.Status)
	}
	if body.Title != http.StatusText(http.StatusTooManyRequests) {
		t.Errorf("want title %q, got %q", http.StatusText(http.StatusTooManyRequests), body.Title)
	}
	if body.Detail == "" {
		t.Error("want non-empty detail")
	}
}

func TestRateLimit_RejectionDoesNotConsumeTokens(t *testing.T) {
	// Rejected requests must not drain the bucket further: once the refill
	// interval passes, the client gets through again. The rate is slow
	// relative to the sleep so the assertions don't depend on tight
	// scheduling margins on a loaded CI runner.
	cfg := middleware.RateLimitConfig{RequestsPerSecond: 10, Burst: 1}
	h := middleware.RateLimit(cfg, discardLogger())(okHandler)

	send := func() int {
		req := newReq(http.MethodGet, "/api")
		req.RemoteAddr = "10.0.0.3:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := send(); code != http.StatusOK {
		t.Fatalf("first request: want 200, got %d", code)
	}
	// Burn several rejections in a row; each must return its reservation.
	for range 3 {
		if code := send(); code != http.StatusTooManyRequests {
			t.Fatalf("want 429 while bucket empty, got %d", code)
		}
	}
	time.Sleep(200 * time.Millisecond) // one token refills in 100 ms at 10 rps
	if code := send(); code != http.StatusOK {
		t.Fatalf("after refill: want 200, got %d", code)
	}
}

func TestWriteProblem_ShapeAndRequestID(t *testing.T) {
	// WriteProblem inside a RequestLogger-wrapped handler must emit the RFC
	// 7807 shape with `instance` populated from the request ID.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		middleware.WriteProblem(w, r, http.StatusNotFound, "job not found")
	})
	h := middleware.RequestLogger(discardLogger())(inner)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newReq(http.MethodGet, "/api/v1/jobs/nope"))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("want application/problem+json, got %q", ct)
	}

	var body struct {
		Type     string `json:"type"`
		Title    string `json:"title"`
		Status   int    `json:"status"`
		Detail   string `json:"detail"`
		Instance string `json:"instance"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Type != "about:blank" {
		t.Errorf("want type about:blank, got %q", body.Type)
	}
	if body.Title != "Not Found" || body.Status != 404 || body.Detail != "job not found" {
		t.Errorf("unexpected fields: %+v", body)
	}
	if body.Instance == "" {
		t.Error("want instance populated from the request ID")
	}
}

func TestRateLimit_DefaultsAppliedWhenZero(t *testing.T) {
	// Zero config should use defaults (20 rps, burst 40) — just ensure it doesn't panic.
	h := middleware.RateLimit(middleware.RateLimitConfig{}, discardLogger())(okHandler)
	req := newReq(http.MethodGet, "/api")
	req.RemoteAddr = "192.168.1.1:8080"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

func TestRateLimit_MalformedRemoteAddr(t *testing.T) {
	// If RemoteAddr can't be parsed, the middleware should still function.
	cfg := middleware.RateLimitConfig{RequestsPerSecond: 100, Burst: 10}
	h := middleware.RateLimit(cfg, discardLogger())(okHandler)

	req := newReq(http.MethodGet, "/api")
	req.RemoteAddr = "not-valid-addr" // no port
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 despite malformed addr, got %d", rr.Code)
	}
}
