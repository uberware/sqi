// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store/fake"
)

// TestNewRouter_HAGate checks the router's HA wiring: no /leaderz and no gate
// without HA; with it, /leaderz mounted and the API gated while a standby.
func TestNewRouter_HAGate(t *testing.T) {
	st := fake.New()
	deps := Deps{Store: st, Products: product.NewCatalog(st)}
	get := func(r http.Handler, path string) int {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		return rr.Code
	}

	t.Run("without HA", func(t *testing.T) {
		r := NewRouter(Config{DisableRateLimit: true}, deps, newTestLogger(), metrics.New(), health.NewRegistry())
		if code := get(r, "/leaderz"); code != http.StatusNotFound {
			t.Errorf("/leaderz = %d, want 404", code)
		}
		if code := get(r, "/api/v1/version"); code != http.StatusOK {
			t.Errorf("/api/v1/version = %d, want 200", code)
		}
	})

	t.Run("with HA", func(t *testing.T) {
		var leader atomic.Bool
		hr := health.NewRegistry()
		hr.SetRoleProvider(func() health.RoleInfo {
			if leader.Load() {
				return health.RoleInfo{Role: "leader", IsLeader: true, Leader: "self", Epoch: 2}
			}
			return health.RoleInfo{Role: "standby", Leader: "alpha", Epoch: 1}
		})
		cfg := Config{DisableRateLimit: true, HA: &HAGate{IsLeader: leader.Load, LeaderName: func() string { return "alpha" }}}
		r := NewRouter(cfg, deps, newTestLogger(), metrics.New(), hr)

		for path, want := range map[string]int{
			"/healthz":        http.StatusOK,
			"/readyz":         http.StatusOK,
			"/leaderz":        http.StatusServiceUnavailable,
			"/metrics":        http.StatusOK,
			"/api/v1/version": http.StatusServiceUnavailable,
		} {
			if code := get(r, path); code != want {
				t.Errorf("standby %s = %d, want %d", path, code, want)
			}
		}
		leader.Store(true)
		for path, want := range map[string]int{
			"/leaderz":        http.StatusOK,
			"/api/v1/version": http.StatusOK,
		} {
			if code := get(r, path); code != want {
				t.Errorf("leader %s = %d, want %d", path, code, want)
			}
		}
	})
}

// TestMountProbes_MountsEveryProbePath: the paths a standby lets through are
// the paths MountProbes mounts, with or without HA.
func TestMountProbes_MountsEveryProbePath(t *testing.T) {
	for _, haGroup := range []bool{false, true} {
		r := chi.NewRouter()
		MountProbes(r, haGroup, health.NewRegistry(), metrics.New())
		var mounted []string
		if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			if method == http.MethodGet && !slices.Contains(mounted, route) {
				mounted = append(mounted, route)
			}
			return nil
		}); err != nil {
			t.Fatalf("walk: %v", err)
		}
		slices.Sort(mounted)
		want := slices.Sorted(slices.Values(ProbePaths))
		if !slices.Equal(mounted, want) {
			t.Errorf("HA %v: GET routes %v, want ProbePaths %v", haGroup, mounted, want)
		}
	}
}
