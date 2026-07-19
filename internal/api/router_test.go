// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for router wiring: REST resource routes are gated by
// middleware.Auth while /healthz and /api/v1/ws are not.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// preflight sends a CORS preflight OPTIONS request for origin against target
// and returns the response so the caller can inspect CORS headers.
func preflight(t *testing.T, target, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, target, nil)
	if err != nil {
		t.Fatalf("build preflight request: %v", err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight OPTIONS %s: %v", target, err)
	}
	return resp
}

// ── CORS / wildcard-drop under auth ────────────────────────────────────────

func TestRouter_AuthOn_DefaultOrigins_WildcardDroppedNotCredentialed(t *testing.T) {
	// The crux of the wildcard problem: cfg.AuthEnabled=true with no
	// CORSOrigins configured (the real-world default — http.cors_origins is
	// empty unless an operator sets it) must never combine AllowCredentials
	// with "*". We drop
	// the wildcard rather than the reverse, so a foreign origin must receive
	// NO Access-Control-Allow-Origin at all — not "*", and not echoed back.
	deps := Deps{Store: fake.New(), Auth: auth.Anonymous(), CookieName: "sqi_session"}
	r := NewRouter(
		Config{DisableRateLimit: true, AuthEnabled: true},
		deps,
		newTestLogger(),
		metrics.New(),
		health.NewRegistry(),
	)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp := preflight(t, srv.URL+"/api/v1/jobs", "https://evil.example")
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty (wildcard must be dropped, not echoed)", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want empty for a disallowed origin", got)
	}
}

func TestRouter_AuthOn_ExplicitOrigin_AllowedWithCredentials(t *testing.T) {
	// An operator who configures an explicit origin gets a working,
	// non-broken credentialed CORS response for that origin.
	deps := Deps{Store: fake.New(), Auth: auth.Anonymous(), CookieName: "sqi_session"}
	r := NewRouter(
		Config{DisableRateLimit: true, AuthEnabled: true, CORSOrigins: []string{"https://farm.example"}},
		deps,
		newTestLogger(),
		metrics.New(),
		health.NewRegistry(),
	)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp := preflight(t, srv.URL+"/api/v1/jobs", "https://farm.example")
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://farm.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want https://farm.example", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

// TestRouter_AuthOff_CORSAndWSUnchanged is the auth-off regression test: with
// AuthEnabled false (the pre-A1 default), CORS must still be the wildcard,
// uncredentialed configuration that shipped before A1, and the /ws upgrade
// must still accept any Origin (InsecureSkipVerify, unaffected by
// wsOriginConfig).
func TestRouter_AuthOff_CORSAndWSUnchanged(t *testing.T) {
	deps := Deps{Store: fake.New(), Auth: auth.Anonymous()}
	r := NewRouter(
		Config{DisableRateLimit: true}, // AuthEnabled defaults to false
		deps,
		newTestLogger(),
		metrics.New(),
		health.NewRegistry(),
	)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp := preflight(t, srv.URL+"/api/v1/jobs", "https://evil.example")
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("auth-off Access-Control-Allow-Origin = %q, want \"*\" (unchanged pre-A1 default)", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("auth-off Access-Control-Allow-Credentials = %q, want empty", got)
	}

	// /ws must still upgrade regardless of Origin when auth is off.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, wsResp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err != nil {
		t.Fatalf("auth-off /ws dial with foreign Origin: %v", err)
	}
	if wsResp != nil && wsResp.Body != nil {
		wsResp.Body.Close()
	}
	if err := conn.CloseNow(); err != nil {
		t.Logf("ws cleanup: CloseNow: %v", err)
	}
}
