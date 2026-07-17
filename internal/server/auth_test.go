// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/session"
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
// bootstrap (seeding the configured admin) and returns a real session
// authenticator rather than the anonymous one.
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
	if _, ok := a.(*session.Authenticator); !ok {
		t.Fatalf("expected *session.Authenticator, got %T", a)
	}

	n, err := st.CountUsers(context.Background())
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected bootstrap to seed 1 admin, got %d", n)
	}
}
