// SPDX-License-Identifier: AGPL-3.0-or-later

package session_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func TestAuthenticate(t *testing.T) {
	st := fake.New()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, store.User{ID: uuid.NewString(), Username: "bob", Role: "operator", PasswordHash: "x"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	tok, err := password.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := st.CreateSession(ctx, store.Session{
		ID: uuid.NewString(), TokenHash: password.HashToken(tok),
		UserID: u.ID, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	authn := session.New(st, session.DefaultCookieName, func() time.Time { return now })

	req := newCookieRequest(t, tok)
	p, err := authn.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Subject != u.ID || p.Kind != auth.KindUser || len(p.Roles) != 1 || p.Roles[0] != "operator" {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if p.DisplayName != u.DisplayName {
		t.Fatalf("DisplayName = %q, want %q", p.DisplayName, u.DisplayName)
	}
	if p.Superuser {
		t.Fatal("session principal must not be superuser")
	}

	// No cookie → error.
	if _, err := authn.Authenticate(&http.Request{Header: http.Header{}}); err == nil {
		t.Fatal("expected error with no cookie")
	}

	// Disabled user → error.
	u.Disabled = true
	if _, err := st.UpdateUser(ctx, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if _, err := authn.Authenticate(newCookieRequest(t, tok)); err == nil {
		t.Fatal("expected error for disabled user")
	}
}

// newCookieRequest builds a GET request carrying the session cookie set to tok.
func newCookieRequest(t *testing.T, tok string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	r.AddCookie(&http.Cookie{Name: session.DefaultCookieName, Value: tok})
	return r
}
