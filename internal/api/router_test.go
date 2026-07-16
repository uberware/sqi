// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for router wiring: REST resource routes are gated by
// middleware.Auth while /healthz and /api/v1/ws are not.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store/fake"
)

func TestRouter_RESTRouteGatedByAuth_WSAndHealthzNotGated(t *testing.T) {
	deps := Deps{
		Store: fake.New(),
		Auth:  stubRouterAuthenticator{err: errors.New("denied")},
	}
	r := NewRouter(
		Config{DisableRateLimit: true},
		deps,
		newTestLogger(),
		metrics.New(),
		health.NewRegistry(),
	)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	// A REST resource route is gated → 401.
	if code := getStatus(t, srv.URL+"/api/v1/jobs"); code != http.StatusUnauthorized {
		t.Errorf("/api/v1/jobs status = %d, want 401", code)
	}
	// Public probe on the root router is never gated → 200.
	if code := getStatus(t, srv.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", code)
	}
	// /ws is not gated by middleware.Auth (its own hook uses the same failing
	// authenticator, so it also 401s here — but crucially not with a problem+json
	// body from the REST group). Assert it is reachable as a route (401 from the
	// WS hook, not 404).
	if code := getStatus(t, srv.URL+"/api/v1/ws"); code == http.StatusNotFound {
		t.Errorf("/api/v1/ws status = 404, want the WS hook to handle it")
	}
}

type stubRouterAuthenticator struct{ err error }

func (s stubRouterAuthenticator) Authenticate(*http.Request) (auth.Principal, error) {
	return auth.Principal{}, s.err
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // simple synchronous test request
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
