// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/session"
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
