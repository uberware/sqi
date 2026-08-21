// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store/fake"
)

// TestNATSAuthDeps_CopiesAllFourSettings covers the SECOND of the three hops
// that carry the nats.auth.* settings from the config file to the REST
// worker-enrollment surface: server.Config -> api.Deps. See
// TestServerConfig_CarriesTheBrokerAuthSettings in cmd/sqi-server for the
// first hop.
func TestNATSAuthDeps_CopiesAllFourSettings(t *testing.T) {
	cfg := Config{
		NATSAuthEnabled:                   true,
		NATSAuthEnrollmentEndpointEnabled: true,
		NATSAuthJoinTokenTTL:              42 * time.Minute,
		NATSAuthJoinTokenSingleUse:        false,
	}
	var deps api.Deps
	natsAuthDeps(cfg, &deps)

	if !deps.NATSAuthEnabled {
		t.Error("deps.NATSAuthEnabled = false, want true")
	}
	if !deps.NATSAuthEnrollmentEndpointEnabled {
		t.Error("deps.NATSAuthEnrollmentEndpointEnabled = false, want true")
	}
	if want := 42 * time.Minute; deps.JoinTokenTTL != want {
		t.Errorf("deps.JoinTokenTTL = %s, want %s", deps.JoinTokenTTL, want)
	}
	if deps.JoinTokenSingleUse {
		t.Error("deps.JoinTokenSingleUse = true, want false")
	}
}

// TestNATSAuthDeps_DoesNotDisturbFieldsItDoesNotOwn confirms natsAuthDeps
// only ever writes its own four fields, mirroring wireAuthDeps's contract:
// deps is built incrementally across several steps in start, so a mapping
// function that clobbers a sibling's field fails silently — the field it
// overwrote simply reverts to its zero value and the server boots anyway.
func TestNATSAuthDeps_DoesNotDisturbFieldsItDoesNotOwn(t *testing.T) {
	deps := api.Deps{CookieName: "sqi_session", SessionTTL: time.Hour}
	natsAuthDeps(Config{}, &deps)

	if deps.CookieName != "sqi_session" {
		t.Errorf("CookieName clobbered: %q", deps.CookieName)
	}
	if deps.SessionTTL != time.Hour {
		t.Errorf("SessionTTL clobbered: %s", deps.SessionTTL)
	}
}

// routeMounted reports whether method+pattern is registered on r.
func routeMounted(t *testing.T, r chi.Router, method, pattern string) bool {
	t.Helper()
	found := false
	err := chi.Walk(r, func(m, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if m == method && route == pattern {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	return found
}

// TestNATSAuthDeps_EndToEnd_EnrollRouteMountedWhenConfigured is the THIRD
// hop: build the exact api.Deps a real boot would produce from a Config with
// broker auth and the enrollment endpoint both on, hand it to the real
// api.NewRouter, and confirm POST /api/v1/workers/enroll is actually live.
//
// Nothing else fails when this hop breaks: the server still boots, `config
// print` still echoes the operator's values, and the route simply never
// mounts.
func TestNATSAuthDeps_EndToEnd_EnrollRouteMountedWhenConfigured(t *testing.T) {
	cfg := Config{
		NATSAuthEnabled:                   true,
		NATSAuthEnrollmentEndpointEnabled: true,
	}
	deps := api.Deps{Store: fake.New(), Auth: auth.Anonymous()}
	natsAuthDeps(cfg, &deps)

	r := api.NewRouter(api.Config{DisableRateLimit: true}, deps, testLogger(), metrics.New(), health.NewRegistry())
	if !routeMounted(t, r, http.MethodPost, "/api/v1/workers/enroll") {
		t.Error("POST /api/v1/workers/enroll is not mounted with NATSAuthEnabled and " +
			"NATSAuthEnrollmentEndpointEnabled both true — the config-to-Deps wiring is broken")
	}
}

// TestNATSAuthDeps_EndToEnd_EnrollRouteAbsentAtDefaults is the companion
// negative case: a server built from DefaultConfig() (broker auth off, the
// v0.3.0 and pre-H1 behavior) must never expose the enrollment endpoint.
func TestNATSAuthDeps_EndToEnd_EnrollRouteAbsentAtDefaults(t *testing.T) {
	deps := api.Deps{Store: fake.New(), Auth: auth.Anonymous()}
	natsAuthDeps(DefaultConfig(), &deps)

	r := api.NewRouter(api.Config{DisableRateLimit: true}, deps, testLogger(), metrics.New(), health.NewRegistry())
	if routeMounted(t, r, http.MethodPost, "/api/v1/workers/enroll") {
		t.Error("POST /api/v1/workers/enroll is mounted at default configuration " +
			"(broker auth off); it must not exist until nats.auth.enabled is turned on")
	}

	// Confirm it collides with /workers/{id} the way router.go's own comment
	// documents (workerenroll_test.go, internal/api, worked around this by
	// walking the route table instead of asserting a status code): a GET or
	// DELETE for that pattern IS registered, just never POST.
	if !routeMounted(t, r, http.MethodGet, "/api/v1/workers/{id}") {
		t.Fatal("test assumption broken: GET /api/v1/workers/{id} is not registered")
	}
}
