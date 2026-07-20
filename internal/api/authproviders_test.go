// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Tests for GET /auth/providers — the login page's discovery endpoint.
//
// Two properties matter here and nowhere else in this package:
//
//   - it answers WITHOUT a session cookie. The login page is unauthenticated by
//     definition, so an authenticated discovery endpoint would be circular and
//     the SSO button would never render;
//   - it leaks nothing. It is reachable by anyone who can reach the server, so
//     the issuer and the client secret must not appear in the body even though
//     both sit in the oidc.Config the handler holds.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/oidc"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store/fake"
)

// Recognizable sentinel values for the two fields that must never reach the
// response body, so asserting their absence is meaningful rather than vacuous.
const (
	providersIssuer = "https://idp.example.com/never-in-a-response-body"
	providersSecret = "client-secret-never-in-a-response-body"
)

func TestGetAuthProviders(t *testing.T) {
	tests := []struct {
		name       string
		oidcOn     bool
		label      string
		wantSSOLen int
		wantLabel  string
	}{
		{
			name:       "sso configured",
			oidcOn:     true,
			label:      "Sign in with Okta",
			wantSSOLen: 1,
			wantLabel:  "Sign in with Okta",
		},
		{
			// An operator who enables SSO without setting button_label still
			// gets a usable button rather than an unlabeled one.
			name:       "sso configured without a button label",
			oidcOn:     true,
			label:      "",
			wantSSOLen: 1,
			wantLabel:  "Sign in with SSO",
		},
		{name: "sso off", oidcOn: false, wantSSOLen: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := oidcTestCfg()
			cfg.Issuer = providersIssuer
			cfg.ClientSecret = providersSecret
			cfg.ButtonLabel = tt.label

			var p oidc.Provider
			if tt.oidcOn {
				p = &fakeOIDCProvider{identity: aliceOIDCIdentity()}
			}
			ts := newOIDCServer(t, fake.New(), p, cfg)

			// Deliberately no session cookie and no Authorization header.
			req, err := http.NewRequestWithContext(
				context.Background(), http.MethodGet, ts.srv.URL+"/api/v1/auth/providers", nil,
			)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /auth/providers: %v", err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			body := string(raw)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", resp.StatusCode, body)
			}
			if strings.Contains(body, providersIssuer) {
				t.Errorf("response leaks the issuer: %q", body)
			}
			if strings.Contains(body, providersSecret) {
				t.Errorf("response leaks the client secret: %q", body)
			}

			var got authProvidersResponse
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode body %q: %v", body, err)
			}

			if !got.Password {
				t.Error("password = false, want true (local login is always offered)")
			}
			if len(got.SSO) != tt.wantSSOLen {
				t.Fatalf("sso = %+v, want %d entries", got.SSO, tt.wantSSOLen)
			}
			if tt.wantSSOLen == 0 {
				return
			}
			if got.SSO[0].Label != tt.wantLabel {
				t.Errorf("sso[0].label = %q, want %q", got.SSO[0].Label, tt.wantLabel)
			}
			if got.SSO[0].ID != "oidc" {
				t.Errorf("sso[0].id = %q, want %q", got.SSO[0].ID, "oidc")
			}
			// The web renders this straight into an anchor's href, so it must
			// be the real login route, not a placeholder.
			if got.SSO[0].LoginURL != "/api/v1/auth/oidc/login" {
				t.Errorf("sso[0].login_url = %q, want %q",
					got.SSO[0].LoginURL, "/api/v1/auth/oidc/login")
			}
		})
	}
}

// TestGetAuthProviders_AuthEnabledFalse pins the auth-off deployment: no
// session, no API key, and none of the auth wiring the cases above build. The
// route is mounted unconditionally in router.go rather than gated on
// cfg.AuthEnabled — "no SSO configured" is itself the answer the login page
// needs even when auth is off entirely — so this must return the same
// no-SSO shape the "sso off" case above does, and the auth-off path must stay
// byte-for-byte unchanged, which this test is here to keep true.
func TestGetAuthProviders_AuthEnabledFalse(t *testing.T) {
	r := NewRouter(
		Config{DisableRateLimit: true, AuthEnabled: false},
		Deps{Store: fake.New(), Auth: auth.Anonymous()},
		newTestLogger(), metrics.New(), health.NewRegistry(),
	)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, srv.URL+"/api/v1/auth/providers", nil,
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /auth/providers: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", resp.StatusCode, string(raw))
	}

	var got authProvidersResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode body %q: %v", string(raw), err)
	}
	if !got.Password {
		t.Error("password = false, want true (local login is always offered, auth on or off)")
	}
	if len(got.SSO) != 0 {
		t.Errorf("sso = %+v, want empty (auth off means no provider is ever built)", got.SSO)
	}
}
