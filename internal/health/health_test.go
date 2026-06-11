// SPDX-License-Identifier: AGPL-3.0-or-later

package health_test

// Tests for Registry.LivenessHandler and Registry.ReadinessHandler.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/health"
)

// ── LivenessHandler ───────────────────────────────────────────────────────────

func TestLivenessHandler_AlwaysOK(t *testing.T) {
	reg := health.NewRegistry()
	h := reg.LivenessHandler()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestLivenessHandler_OKEvenWithFailingCheckers(t *testing.T) {
	reg := health.NewRegistry()
	// Register a failing checker — liveness should still return 200.
	reg.Register("broken", health.CheckerFunc(func(_ context.Context) error {
		return errors.New("something is wrong")
	}))

	h := reg.LivenessHandler()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("liveness should always be 200, got %d", rr.Code)
	}
}

// ── ReadinessHandler ──────────────────────────────────────────────────────────

func TestReadinessHandler_NoCheckers_IsReady(t *testing.T) {
	reg := health.NewRegistry()
	h := reg.ReadinessHandler()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 with no checkers, got %d", rr.Code)
	}
}

func TestReadinessHandler_AllCheckersPass(t *testing.T) {
	reg := health.NewRegistry()
	reg.Register("db", health.CheckerFunc(func(_ context.Context) error { return nil }))
	reg.Register("nats", health.CheckerFunc(func(_ context.Context) error { return nil }))

	h := reg.ReadinessHandler()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 when all pass, got %d", rr.Code)
	}

	var body struct {
		Status string         `json:"status"`
		Checks map[string]any `json:"checks"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
}

func TestReadinessHandler_OneCheckerFails(t *testing.T) {
	reg := health.NewRegistry()
	reg.Register("db", health.CheckerFunc(func(_ context.Context) error { return nil }))
	reg.Register("nats", health.CheckerFunc(func(_ context.Context) error {
		return errors.New("connection refused")
	}))

	h := reg.ReadinessHandler()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when a checker fails, got %d", rr.Code)
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
}

func TestReadinessHandler_CheckerReplacedByRegister(t *testing.T) {
	reg := health.NewRegistry()
	// Register a failing checker, then replace it with a passing one.
	reg.Register("service", health.CheckerFunc(func(_ context.Context) error {
		return errors.New("down")
	}))
	reg.Register("service", health.CheckerFunc(func(_ context.Context) error {
		return nil // healthy now
	}))

	h := reg.ReadinessHandler()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 after checker replaced, got %d", rr.Code)
	}
}

func TestCheckerFunc_ImplementsChecker(t *testing.T) {
	called := false
	var c health.Checker = health.CheckerFunc(func(_ context.Context) error {
		called = true
		return nil
	})

	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("CheckerFunc was not invoked")
	}
}
