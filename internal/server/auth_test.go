// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/config"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// anonymousRequest returns a Principal by exercising Authenticate on a bare
// request with no credentials attached — the shape every unauthenticated
// request has.
func anonymousRequest() *http.Request {
	return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
}

// TestSelectAuth_Disabled is the auth-off regression test: it must be
// byte-for-byte pre-A1 behavior. Even with bootstrap credentials configured,
// auth disabled must select the anonymous superuser authenticator and must
// NOT create any user or touch the store at all.
func TestSelectAuth_Disabled(t *testing.T) {
	st := fake.New()
	s := &Server{
		cfg: Config{
			AuthEnabled:           false,
			AuthBootstrapUsername: "admin",
			AuthBootstrapPassword: "pw",
		},
		store:  st,
		logger: testLogger(),
	}

	a, err := s.selectAuth(context.Background())
	if err != nil {
		t.Fatalf("selectAuth: %v", err)
	}

	p, err := a.Authenticate(anonymousRequest())
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Kind != auth.KindAnonymous || !p.Superuser {
		t.Fatalf("expected anonymous superuser principal, got %+v", p)
	}

	n, err := st.CountUsers(context.Background())
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 0 {
		t.Fatalf("auth-off bootstrap must never create a user, got %d users", n)
	}
}

// TestSelectAuth_Enabled asserts that with auth enabled, selectAuth runs
// bootstrap (seeding the configured admin) and returns a real
// Chain(apikey, session) authenticator rather than the anonymous one.
func TestSelectAuth_Enabled(t *testing.T) {
	st := fake.New()
	s := &Server{
		cfg: Config{
			AuthEnabled:           true,
			AuthCookieName:        session.DefaultCookieName,
			AuthBootstrapUsername: "admin",
			AuthBootstrapPassword: "pw",
		},
		store:  st,
		logger: testLogger(),
	}

	a, err := s.selectAuth(context.Background())
	if err != nil {
		t.Fatalf("selectAuth: %v", err)
	}
	// A credential-less request must fail: the anonymous authenticator
	// always succeeds, so this distinguishes the real chain from it
	// without depending on the chain's concrete (unexported) type.
	if p, aerr := a.Authenticate(anonymousRequest()); aerr == nil {
		t.Fatalf("expected credential-less request to fail authentication, got %+v", p)
	}

	n, err := st.CountUsers(context.Background())
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected bootstrap to seed 1 admin, got %d", n)
	}
}

// TestSelectAuth_AuthenticatesAPIKey asserts that with auth enabled,
// selectAuth wires an authenticator that resolves a Bearer API key — not
// just a session cookie. This is the Chain(apikey, session) composition:
// key-first, session-fallback.
func TestSelectAuth_AuthenticatesAPIKey(t *testing.T) {
	ctx := context.Background()
	st := fake.New()

	u, err := st.CreateUser(ctx, store.User{
		ID:           uuid.NewString(),
		Username:     "svc",
		PasswordHash: "x",
		Role:         "operator",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	raw := "sqi_" + uuid.NewString()
	if _, err := st.CreateAPIKey(ctx, store.APIKey{
		ID:        uuid.NewString(),
		UserID:    u.ID,
		Name:      "k",
		Prefix:    raw[:12],
		TokenHash: password.HashToken(raw),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	s := &Server{
		cfg: Config{
			AuthEnabled:           true,
			AuthCookieName:        session.DefaultCookieName,
			AuthBootstrapUsername: "admin",
			AuthBootstrapPassword: "pw",
		},
		store:  st,
		logger: testLogger(),
	}

	a, err := s.selectAuth(ctx)
	if err != nil {
		t.Fatalf("selectAuth: %v", err)
	}

	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/jobs", nil)
	r.Header.Set("Authorization", "Bearer "+raw)

	p, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Subject != u.ID || p.Kind != auth.KindAPIKey {
		t.Fatalf("bearer auth via chain failed: %+v", p)
	}
}

// TestSelectAuth_LDAPDisabledInjectsNoVerifier asserts that leaving
// auth.ldap.enabled off produces no verifier at all, so the login path keeps
// its pre-C1 behavior rather than merely being configured not to use it.
func TestSelectAuth_LDAPDisabledInjectsNoVerifier(t *testing.T) {
	s := &Server{
		cfg:    Config{AuthEnabled: true},
		store:  fake.New(),
		logger: testLogger(),
	}

	v, err := s.buildLDAPVerifier(context.Background())
	if err != nil {
		t.Fatalf("buildLDAPVerifier: %v", err)
	}
	if v != nil {
		t.Errorf("expected nil verifier when ldap is disabled, got %T", v)
	}
}

// TestSelectAuth_AuthDisabledInjectsNoVerifier is the auth-off half of the
// same invariant: auth.enabled=false must produce no verifier even when
// auth.ldap.* is fully and validly configured with enabled=true. Without the
// AuthEnabled arm of buildLDAPVerifier's guard, an auth-off server would hold
// a live directory verifier and POST /auth/login — which is mounted
// unconditionally and has no AuthEnabled check of its own — would bind
// against the directory and just-in-time provision accounts on a server the
// operator believes has authentication turned off entirely.
func TestSelectAuth_AuthDisabledInjectsNoVerifier(t *testing.T) {
	cfg := Config{AuthEnabled: false}
	cfg.AuthLDAP = config.LDAPConfig{
		Enabled: true,
		URL:     "ldap://dc.example.com:389",
		Timeout: 10 * time.Second,
		BaseDN:  "DC=example,DC=com", BindDN: "CN=svc,DC=example,DC=com",
		UserFilter: "(sAMAccountName=%s)",
		RoleSource: "directory", DefaultRole: "read-only",
	}
	s := &Server{
		cfg:    cfg,
		store:  fake.New(),
		logger: testLogger(),
	}

	v, err := s.buildLDAPVerifier(context.Background())
	if err != nil {
		t.Fatalf("buildLDAPVerifier: %v", err)
	}
	if v != nil {
		t.Errorf("expected nil verifier when auth is disabled, got %T", v)
	}
}

// TestSelectAuth_LDAPBadCAFileFailsBoot: a misconfigured directory must abort
// boot rather than yield a server that silently cannot authenticate any
// directory account.
func TestSelectAuth_LDAPBadCAFileFailsBoot(t *testing.T) {
	cfg := Config{AuthEnabled: true}
	cfg.AuthLDAP = config.LDAPConfig{
		Enabled: true,
		URL:     "ldaps://dc.example.com:636",
		Timeout: 10 * time.Second,
		BaseDN:  "DC=example,DC=com", BindDN: "CN=svc,DC=example,DC=com",
		UserFilter: "(sAMAccountName=%s)",
		RoleSource: "directory", DefaultRole: "read-only",
		CAFile: filepath.Join(t.TempDir(), "does-not-exist.pem"),
	}
	s := &Server{
		cfg:    cfg,
		store:  fake.New(),
		logger: testLogger(),
	}

	if _, err := s.buildLDAPVerifier(context.Background()); err == nil {
		t.Fatal("expected an error for an unreadable ca_file, got nil")
	}
}

// TestSelectAuth_LDAPEnabledBuildsVerifier asserts that a valid, enabled LDAP
// config produces a non-nil verifier.
func TestSelectAuth_LDAPEnabledBuildsVerifier(t *testing.T) {
	cfg := Config{AuthEnabled: true}
	cfg.AuthLDAP = config.LDAPConfig{
		Enabled: true,
		URL:     "ldap://dc.example.com:389",
		Timeout: 10 * time.Second,
		BaseDN:  "DC=example,DC=com", BindDN: "CN=svc,DC=example,DC=com",
		UserFilter: "(sAMAccountName=%s)",
		RoleSource: "directory", DefaultRole: "read-only",
	}
	s := &Server{
		cfg:    cfg,
		store:  fake.New(),
		logger: testLogger(),
	}

	v, err := s.buildLDAPVerifier(context.Background())
	if err != nil {
		t.Fatalf("buildLDAPVerifier: %v", err)
	}
	if v == nil {
		t.Fatal("expected a verifier when ldap is enabled")
	}
}

// TestToLDAPConfig_MapsEveryField pins the hand-written field-by-field
// conversion in toLDAPConfig. Every source field below carries a distinct,
// recognizable value (booleans excepted, where "distinct" is impossible) so
// that a swapped assignment — e.g. sourcing BindDN from BaseDN — shows up as
// a mismatch naming the field, rather than being masked by two fields
// sharing a value or a zero value that can't distinguish "mapped" from "not
// mapped at all".
func TestToLDAPConfig_MapsEveryField(t *testing.T) {
	logger := testLogger()
	cfg := config.LDAPConfig{
		// Enabled is intentionally excluded from this fixture's assertions:
		// ldap.Config has no Enabled field by design (see the doc comment on
		// toLDAPConfig) because the enabled gate is applied by the caller
		// before toLDAPConfig is ever invoked. Still set here so the fixture
		// has no zero-valued fields.
		Enabled:         true,
		URL:             "ldaps://field-url.example.com:636",
		StartTLS:        true,
		TLSSkipVerify:   true,
		CAFile:          "/field/ca-file.pem",
		Timeout:         37 * time.Second,
		BindDN:          "CN=field-bind-dn,DC=example,DC=com",
		BindPassword:    "field-bind-password",
		BaseDN:          "DC=field-base-dn,DC=example,DC=com",
		UserFilter:      "(field-user-filter=%s)",
		NestedGroups:    true,
		UserDNTemplate:  "CN=%s,OU=field-user-dn-template,DC=example,DC=com",
		UsernameAttr:    "fieldUsernameAttr",
		DisplayNameAttr: "fieldDisplayNameAttr",
		RoleSource:      "field-role-source",
		RoleMap: []config.RoleMappingConfig{
			{Group: "CN=field-group-one,DC=example,DC=com", Role: "field-role-one"},
			{Group: "CN=field-group-two,DC=example,DC=com", Role: "field-role-two"},
		},
		DefaultRole: "field-default-role",
	}

	got := toLDAPConfig(cfg, logger)

	if got.URL != cfg.URL {
		t.Errorf("URL: got %q, want %q", got.URL, cfg.URL)
	}
	if got.StartTLS != cfg.StartTLS {
		t.Errorf("StartTLS: got %v, want %v", got.StartTLS, cfg.StartTLS)
	}
	if got.TLSSkipVerify != cfg.TLSSkipVerify {
		t.Errorf("TLSSkipVerify: got %v, want %v", got.TLSSkipVerify, cfg.TLSSkipVerify)
	}
	if got.CAFile != cfg.CAFile {
		t.Errorf("CAFile: got %q, want %q", got.CAFile, cfg.CAFile)
	}
	if got.Timeout != cfg.Timeout {
		t.Errorf("Timeout: got %v, want %v", got.Timeout, cfg.Timeout)
	}
	if got.BindDN != cfg.BindDN {
		t.Errorf("BindDN: got %q, want %q", got.BindDN, cfg.BindDN)
	}
	if got.BindPassword != cfg.BindPassword {
		t.Errorf("BindPassword: got %q, want %q", got.BindPassword, cfg.BindPassword)
	}
	if got.BaseDN != cfg.BaseDN {
		t.Errorf("BaseDN: got %q, want %q", got.BaseDN, cfg.BaseDN)
	}
	if got.UserFilter != cfg.UserFilter {
		t.Errorf("UserFilter: got %q, want %q", got.UserFilter, cfg.UserFilter)
	}
	if got.NestedGroups != cfg.NestedGroups {
		t.Errorf("NestedGroups: got %v, want %v", got.NestedGroups, cfg.NestedGroups)
	}
	if got.UserDNTemplate != cfg.UserDNTemplate {
		t.Errorf("UserDNTemplate: got %q, want %q", got.UserDNTemplate, cfg.UserDNTemplate)
	}
	if got.UsernameAttr != cfg.UsernameAttr {
		t.Errorf("UsernameAttr: got %q, want %q", got.UsernameAttr, cfg.UsernameAttr)
	}
	if got.DisplayNameAttr != cfg.DisplayNameAttr {
		t.Errorf("DisplayNameAttr: got %q, want %q", got.DisplayNameAttr, cfg.DisplayNameAttr)
	}
	if got.RoleSource != cfg.RoleSource {
		t.Errorf("RoleSource: got %q, want %q", got.RoleSource, cfg.RoleSource)
	}
	if got.DefaultRole != cfg.DefaultRole {
		t.Errorf("DefaultRole: got %q, want %q", got.DefaultRole, cfg.DefaultRole)
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
