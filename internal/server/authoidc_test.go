// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// Tests for the OIDC/SSO half of wireAuthDeps (Phase 3, component C2).
//
// Everything below C2 built — the provider, the state cookie, the login and
// callback routes — is unreachable in a real binary unless this wiring is
// correct, and every way it can be wrong is silent: a missing state key makes
// the routes vanish with a 404, and a transposed field in toOIDCConfig
// compiles, passes every other test, and fails only against a real provider.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/api"
	"github.com/uberware/sqi/internal/auth/oidc"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store/fake"
)

// validOIDCConfig is a minimally complete, enabled SSO configuration.
func validOIDCConfig() config.OIDCConfig {
	return config.OIDCConfig{
		Enabled:      true,
		Issuer:       "https://idp.example.com",
		ClientID:     "sqi",
		ClientSecret: "s3cret",
		RedirectURL:  "https://sqi.example.com/api/v1/auth/oidc/callback",
		RoleSource:   oidc.RoleSourceDirectory,
		DefaultRole:  "read-only",
		ReauthMode:   oidc.ReauthAfterLogout,
		LogoutMode:   oidc.LogoutLocal,
	}
}

// TestBuildOIDCProvider_DisabledInjectsNoProvider: leaving auth.oidc.enabled
// off must produce no provider at all, so the router keeps its pre-C2 shape
// rather than merely being configured not to use SSO.
func TestBuildOIDCProvider_DisabledInjectsNoProvider(t *testing.T) {
	s := &Server{cfg: Config{AuthEnabled: true}, store: fake.New(), logger: testLogger()}

	p, key, err := s.buildOIDCProvider(context.Background())
	if err != nil {
		t.Fatalf("buildOIDCProvider: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider when oidc is disabled, got %T", p)
	}
	if key != nil {
		t.Errorf("expected no state key when oidc is disabled, got %d bytes", len(key))
	}
}

// TestBuildOIDCProvider_AuthDisabledInjectsNoProvider is the auth-off half of
// the same invariant: auth.enabled=false must produce no provider even when
// auth.oidc.* is fully configured with enabled=true. Without the AuthEnabled
// arm of the guard, an operator who believes authentication is off entirely
// would still be running public SSO routes that just-in-time provision
// accounts and mint session cookies.
func TestBuildOIDCProvider_AuthDisabledInjectsNoProvider(t *testing.T) {
	cfg := Config{AuthEnabled: false}
	cfg.AuthOIDC = validOIDCConfig()
	s := &Server{cfg: cfg, store: fake.New(), logger: testLogger()}

	p, key, err := s.buildOIDCProvider(context.Background())
	if err != nil {
		t.Fatalf("buildOIDCProvider: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider when auth is disabled, got %T", p)
	}
	if key != nil {
		t.Errorf("expected no state key when auth is disabled, got %d bytes", len(key))
	}
}

// TestBuildOIDCProvider_EnabledBuildsProviderAndKey asserts the enabled path
// yields both halves. The key is checked for non-emptiness specifically: the
// router refuses to register the SSO routes without it, and an empty key would
// also produce a state MAC anyone can compute.
func TestBuildOIDCProvider_EnabledBuildsProviderAndKey(t *testing.T) {
	cfg := Config{AuthEnabled: true}
	cfg.AuthOIDC = validOIDCConfig()
	s := &Server{cfg: cfg, store: fake.New(), logger: testLogger()}

	p, key, err := s.buildOIDCProvider(context.Background())
	if err != nil {
		t.Fatalf("buildOIDCProvider: %v", err)
	}
	if p == nil {
		t.Fatal("expected a provider when oidc is enabled")
	}
	if len(key) == 0 {
		t.Fatal("expected a non-empty state signing key when oidc is enabled")
	}
}

// TestBuildOIDCProvider_UnreachableIssuerStillBoots pins the deliberate
// asymmetry with buildLDAPVerifier: construction performs no network I/O, so
// an issuer that does not resolve at all must not stop the server from
// starting. A render farm's scheduler going down because the identity provider
// is briefly unreachable would make an external service a hard dependency of
// the whole system rather than of SSO alone.
func TestBuildOIDCProvider_UnreachableIssuerStillBoots(t *testing.T) {
	cfg := Config{AuthEnabled: true}
	cfg.AuthOIDC = validOIDCConfig()
	// RFC 6761 reserves .invalid; this name is guaranteed never to resolve.
	cfg.AuthOIDC.Issuer = "https://idp.invalid"
	s := &Server{cfg: cfg, store: fake.New(), logger: testLogger()}

	p, _, err := s.buildOIDCProvider(context.Background())
	if err != nil {
		t.Fatalf("buildOIDCProvider must not perform discovery at boot, got error: %v", err)
	}
	if p == nil {
		t.Fatal("expected a provider even with an unreachable issuer")
	}
}

// TestWireAuthDeps_SSORoutesAreRegistered is the end-to-end wiring assertion:
// a fully-configured SSO deployment must actually serve the SSO routes.
//
// This exists because the failure it catches is silent. NewRouter registers
// /auth/oidc/login only when BOTH the provider and a non-empty state key are
// present, so forgetting to assign deps.OIDCStateKey — a single line in
// wireAuthDeps, with nothing else in the system referencing it — yields a
// server that boots cleanly, logs "auth: oidc enabled", and 404s the login
// route. No other test in either package would notice.
func TestWireAuthDeps_SSORoutesAreRegistered(t *testing.T) {
	cfg := Config{AuthEnabled: true, AuthCookieName: "sqi_session"}
	cfg.AuthOIDC = validOIDCConfig()
	st := fake.New()
	s := &Server{cfg: cfg, store: st, logger: testLogger()}

	deps := api.Deps{Store: st, Products: product.NewCatalog(st)}
	if err := s.wireAuthDeps(context.Background(), &deps); err != nil {
		t.Fatalf("wireAuthDeps: %v", err)
	}
	if deps.OIDCProvider == nil {
		t.Fatal("wireAuthDeps left OIDCProvider nil for an enabled SSO config")
	}
	if len(deps.OIDCStateKey) == 0 {
		t.Fatal("wireAuthDeps left OIDCStateKey empty; the router will not register the SSO routes")
	}
	// The config must arrive alongside the provider — the handlers read claim
	// names and modes from it, and a zero value there would silently disable
	// role mapping.
	if deps.OIDCConfig.Issuer != cfg.AuthOIDC.Issuer {
		t.Errorf("OIDCConfig.Issuer = %q, want %q", deps.OIDCConfig.Issuer, cfg.AuthOIDC.Issuer)
	}

	r := api.NewRouter(
		api.Config{DisableRateLimit: true, AuthEnabled: true},
		deps,
		testLogger(),
		metrics.New(),
		health.NewRegistry(),
	)

	for _, path := range []string{"/api/v1/auth/oidc/login", "/api/v1/auth/oidc/callback"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s returned 404: the SSO routes are not registered on a fully-configured "+
				"SSO deployment", path)
		}
	}

	// /auth/providers is mounted unconditionally and must answer without any
	// credential — it is what the login page reads to know the button exists.
	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodGet, "/api/v1/auth/providers", nil,
	)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/v1/auth/providers (no credential) = %d, want 200", rec.Code)
	}
}

// TestToOIDCConfig_MapsEveryField pins the hand-written field-by-field
// conversion in toOIDCConfig, exactly as TestToLDAPConfig_MapsEveryField pins
// its LDAP twin. Every source field carries a distinct, recognizable value so
// that a swapped assignment — sourcing GroupsClaim from UsernameClaim, say —
// shows up as a mismatch naming the field rather than being masked by two
// fields that happen to share a value.
//
// This matters more here than the compiler suggests: nearly every field is a
// string, so a transposition is invisible to the type checker and would
// surface only against a real identity provider, as a login that fails or —
// worse — silently maps everyone to the wrong role.
func TestToOIDCConfig_MapsEveryField(t *testing.T) {
	logger := testLogger()
	cfg := config.OIDCConfig{
		// Enabled is intentionally excluded from the assertions below:
		// oidc.Config has no Enabled field by design (see toOIDCConfig)
		// because the gate is applied before it is ever called. Set here only
		// so the fixture has no zero-valued fields.
		Enabled:               true,
		Issuer:                "https://field-issuer.example.com",
		ClientID:              "field-client-id",
		ClientSecret:          "field-client-secret",
		RedirectURL:           "https://field-redirect-url.example.com/cb",
		Scopes:                []string{"field-scope-one", "field-scope-two"},
		UsernameClaim:         "field_username_claim",
		DisplayNameClaim:      "field_display_name_claim",
		GroupsClaim:           "field_groups_claim",
		RoleSource:            "field-role-source",
		DefaultRole:           "field-default-role",
		ReauthMode:            "field-reauth-mode",
		LogoutMode:            "field-logout-mode",
		PostLogoutRedirectURL: "https://field-post-logout.example.com/bye",
		ButtonLabel:           "field-button-label",
		RoleMap: []config.RoleMappingConfig{
			{Group: "field-group-one", Role: "field-role-one"},
			{Group: "field-group-two", Role: "field-role-two"},
		},
	}

	got := toOIDCConfig(cfg, logger)

	if got.Issuer != cfg.Issuer {
		t.Errorf("Issuer: got %q, want %q", got.Issuer, cfg.Issuer)
	}
	if got.ClientID != cfg.ClientID {
		t.Errorf("ClientID: got %q, want %q", got.ClientID, cfg.ClientID)
	}
	if got.ClientSecret != cfg.ClientSecret {
		t.Errorf("ClientSecret: got %q, want %q", got.ClientSecret, cfg.ClientSecret)
	}
	if got.RedirectURL != cfg.RedirectURL {
		t.Errorf("RedirectURL: got %q, want %q", got.RedirectURL, cfg.RedirectURL)
	}
	if len(got.Scopes) != len(cfg.Scopes) {
		t.Fatalf("Scopes: got %v, want %v", got.Scopes, cfg.Scopes)
	}
	for i, want := range cfg.Scopes {
		if got.Scopes[i] != want {
			t.Errorf("Scopes[%d]: got %q, want %q", i, got.Scopes[i], want)
		}
	}
	if got.UsernameClaim != cfg.UsernameClaim {
		t.Errorf("UsernameClaim: got %q, want %q", got.UsernameClaim, cfg.UsernameClaim)
	}
	if got.DisplayNameClaim != cfg.DisplayNameClaim {
		t.Errorf("DisplayNameClaim: got %q, want %q", got.DisplayNameClaim, cfg.DisplayNameClaim)
	}
	// GroupsClaim not carried across (or sourced from the wrong claim) means
	// every login presents no groups, so role_source=directory silently
	// demotes the entire organization to default_role — the exact failure C1
	// shipped with against a real OpenLDAP server.
	if got.GroupsClaim != cfg.GroupsClaim {
		t.Errorf("GroupsClaim: got %q, want %q", got.GroupsClaim, cfg.GroupsClaim)
	}
	if got.RoleSource != cfg.RoleSource {
		t.Errorf("RoleSource: got %q, want %q", got.RoleSource, cfg.RoleSource)
	}
	if got.DefaultRole != cfg.DefaultRole {
		t.Errorf("DefaultRole: got %q, want %q", got.DefaultRole, cfg.DefaultRole)
	}
	if got.ReauthMode != cfg.ReauthMode {
		t.Errorf("ReauthMode: got %q, want %q", got.ReauthMode, cfg.ReauthMode)
	}
	if got.LogoutMode != cfg.LogoutMode {
		t.Errorf("LogoutMode: got %q, want %q", got.LogoutMode, cfg.LogoutMode)
	}
	if got.PostLogoutRedirectURL != cfg.PostLogoutRedirectURL {
		t.Errorf("PostLogoutRedirectURL: got %q, want %q",
			got.PostLogoutRedirectURL, cfg.PostLogoutRedirectURL)
	}
	if got.ButtonLabel != cfg.ButtonLabel {
		t.Errorf("ButtonLabel: got %q, want %q", got.ButtonLabel, cfg.ButtonLabel)
	}
	if got.Logger != logger {
		t.Errorf("Logger: got %p, want the same logger pointer passed in (%p)", got.Logger, logger)
	}

	// RoleMap: two entries with distinct Group/Role values each, so a
	// Group/Role swap inside the per-element conversion loop is caught, and
	// checked in order, since RoleMap order is the operator's precedence
	// mechanism and a reordering would silently change who gets which role.
	if len(got.RoleMap) != len(cfg.RoleMap) {
		t.Fatalf("RoleMap: got %d entries, want %d", len(got.RoleMap), len(cfg.RoleMap))
	}
	for i, want := range cfg.RoleMap {
		if got.RoleMap[i].Group != want.Group {
			t.Errorf("RoleMap[%d].Group: got %q, want %q", i, got.RoleMap[i].Group, want.Group)
		}
		if got.RoleMap[i].Role != want.Role {
			t.Errorf("RoleMap[%d].Role: got %q, want %q", i, got.RoleMap[i].Role, want.Role)
		}
	}
}
