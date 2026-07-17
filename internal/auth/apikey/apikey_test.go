// SPDX-License-Identifier: AGPL-3.0-or-later

package apikey_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/apikey"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func reqWithBearer(t *testing.T, tok string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/jobs", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func seedKey(t *testing.T, st *fake.Store, role string) (string, store.User) {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, store.User{
		ID: uuid.NewString(), Username: "svc-" + uuid.NewString(),
		PasswordHash: "x", Role: role,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw := "sqi_" + uuid.NewString()
	if _, err := st.CreateAPIKey(ctx, store.APIKey{
		ID: uuid.NewString(), UserID: u.ID, Name: "k",
		Prefix: raw[:12], TokenHash: password.HashToken(raw),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return raw, u
}

func TestAuthenticate_ValidKey(t *testing.T) {
	st := fake.New()
	raw, u := seedKey(t, st, "operator")
	a := apikey.New(st, nil)

	p, err := a.Authenticate(reqWithBearer(t, raw))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Subject != u.ID || p.Kind != auth.KindAPIKey || len(p.Roles) != 1 || p.Roles[0] != "operator" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

func TestAuthenticate_NoHeader_ErrNoCredential(t *testing.T) {
	a := apikey.New(fake.New(), nil)
	if _, err := a.Authenticate(reqWithBearer(t, "")); !errors.Is(err, apikey.ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
}

func TestAuthenticate_UnknownKey_Rejected(t *testing.T) {
	a := apikey.New(fake.New(), nil)
	if _, err := a.Authenticate(reqWithBearer(t, "sqi_nope")); err == nil || errors.Is(err, apikey.ErrNoCredential) {
		t.Fatalf("unknown key should be a hard error, got %v", err)
	}
}

func TestAuthenticate_RevokedKey_Rejected(t *testing.T) {
	st := fake.New()
	raw, u := seedKey(t, st, "user")
	// Revoke it.
	keys, err := st.ListAPIKeysForUser(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("ListAPIKeysForUser: %v", err)
	}
	if err := st.RevokeAPIKey(context.Background(), keys[0].ID, u.ID, time.Now().UTC()); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	a := apikey.New(st, nil)
	if _, err := a.Authenticate(reqWithBearer(t, raw)); err == nil {
		t.Fatalf("revoked key should be rejected")
	}
}

func TestAuthenticate_DisabledUser_Rejected(t *testing.T) {
	st := fake.New()
	raw, u := seedKey(t, st, "user")
	u.Disabled = true
	if _, err := st.UpdateUser(context.Background(), u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	a := apikey.New(st, nil)
	if _, err := a.Authenticate(reqWithBearer(t, raw)); err == nil {
		t.Fatalf("disabled user's key should be rejected")
	}
}

func TestAuthenticate_TouchesLastUsed_Throttled(t *testing.T) {
	st := fake.New()
	raw, u := seedKey(t, st, "user")
	base := time.Now().UTC()
	clock := base
	a := apikey.New(st, func() time.Time { return clock })

	// First use writes last_used_at.
	if _, err := a.Authenticate(reqWithBearer(t, raw)); err != nil {
		t.Fatalf("auth 1: %v", err)
	}
	k1, err := st.GetAPIKeyByTokenHash(context.Background(), password.HashToken(raw), clock)
	if err != nil {
		t.Fatalf("GetAPIKeyByTokenHash (1): %v", err)
	}
	if k1.LastUsedAt == nil || !k1.LastUsedAt.Equal(base) {
		t.Fatalf("first use should set last_used_at to base, got %v", k1.LastUsedAt)
	}

	// Second use 5s later (< 1m throttle) must NOT advance last_used_at.
	clock = base.Add(5 * time.Second)
	if _, err := a.Authenticate(reqWithBearer(t, raw)); err != nil {
		t.Fatalf("auth 2: %v", err)
	}
	k2, err := st.GetAPIKeyByTokenHash(context.Background(), password.HashToken(raw), clock)
	if err != nil {
		t.Fatalf("GetAPIKeyByTokenHash (2): %v", err)
	}
	if !k2.LastUsedAt.Equal(base) {
		t.Fatalf("within-throttle use should not advance last_used_at, got %v", k2.LastUsedAt)
	}

	// Third use 2m later must advance it.
	clock = base.Add(2 * time.Minute)
	if _, err := a.Authenticate(reqWithBearer(t, raw)); err != nil {
		t.Fatalf("auth 3: %v", err)
	}
	k3, err := st.GetAPIKeyByTokenHash(context.Background(), password.HashToken(raw), clock)
	if err != nil {
		t.Fatalf("GetAPIKeyByTokenHash (3): %v", err)
	}
	if !k3.LastUsedAt.Equal(clock) {
		t.Fatalf("post-throttle use should advance last_used_at to %v, got %v", clock, k3.LastUsedAt)
	}
	_ = u
}
