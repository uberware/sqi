// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/uberware/sqi/internal/middleware"
)

// probes is what the router passes: the paths a standby serves.
var probes = []string{"/healthz", "/readyz", "/leaderz", "/metrics"}

func TestStandby(t *testing.T) {
	tests := []struct {
		name      string
		leader    bool
		path      string
		wantCode  int
		wantInner bool
	}{
		{"standby gates the API", false, "/api/v1/version", http.StatusServiceUnavailable, false},
		{"standby gates the web UI", false, "/", http.StatusServiceUnavailable, false},
		{"standby gates a UI route", false, "/jobs/123", http.StatusServiceUnavailable, false},
		{"standby gates the WebSocket", false, "/api/v1/ws", http.StatusServiceUnavailable, false},
		{"standby serves /healthz", false, "/healthz", http.StatusTeapot, true},
		{"standby serves /readyz", false, "/readyz", http.StatusTeapot, true},
		{"standby serves /leaderz", false, "/leaderz", http.StatusTeapot, true},
		{"standby serves /metrics", false, "/metrics", http.StatusTeapot, true},
		{"a prefix of a probe is not the probe", false, "/healthzz", http.StatusServiceUnavailable, false},
		{"a path under a probe is not the probe", false, "/metrics/x", http.StatusServiceUnavailable, false},
		{"leader passes the API", true, "/api/v1/version", http.StatusTeapot, true},
		{"leader passes the UI", true, "/", http.StatusTeapot, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called atomic.Bool
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called.Store(true)
				w.WriteHeader(http.StatusTeapot)
			})
			h := middleware.Standby(func() bool { return tt.leader }, func() string { return "alpha" }, probes)(inner)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tt.wantCode || called.Load() != tt.wantInner {
				t.Fatalf("%s: code %d inner %v; want %d inner %v", tt.path, rr.Code, called.Load(), tt.wantCode, tt.wantInner)
			}
			if tt.wantInner {
				return
			}
			if got := rr.Header().Get("Retry-After"); got != "2" {
				t.Errorf("Retry-After = %q, want 2", got)
			}
			if got := rr.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			var body map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", rr.Body.String(), err)
			}
			if body["error"] != "standby" || body["leader"] != "alpha" || len(body) != 2 {
				t.Errorf("body = %v; want error standby, leader alpha", body)
			}
		})
	}
}

// TestStandby_NoLeaderKnown names no leader rather than inventing one.
func TestStandby_NoLeaderKnown(t *testing.T) {
	h := middleware.Standby(func() bool { return false }, func() string { return "" }, probes)(http.NotFoundHandler())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/jobs", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != `{"error":"standby","leader":""}` {
		t.Fatalf("= %d %s; want 503 with an empty leader", rr.Code, rr.Body.String())
	}
}

// TestStandby_ReadsRolePerRequest: a promotion opens the API without
// rebuilding the handler.
func TestStandby_ReadsRolePerRequest(t *testing.T) {
	var leader atomic.Bool
	h := middleware.Standby(leader.Load, func() string { return "self" }, probes)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	)
	get := func() int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/jobs", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := get(); got != http.StatusServiceUnavailable {
		t.Fatalf("before promotion = %d, want 503", got)
	}
	leader.Store(true)
	if got := get(); got != http.StatusOK {
		t.Fatalf("after promotion = %d, want 200", got)
	}
}

// TestStandby_ServesOnlyTheGivenProbes: the probes are the caller's list, so
// a path left out of it is gated like any other.
func TestStandby_ServesOnlyTheGivenProbes(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := middleware.Standby(func() bool { return false }, func() string { return "alpha" }, []string{"/healthz"})(inner)
	for path, want := range map[string]int{
		"/healthz": http.StatusTeapot,
		"/metrics": http.StatusServiceUnavailable,
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rr.Code != want {
			t.Errorf("%s = %d, want %d", path, rr.Code, want)
		}
	}
}
