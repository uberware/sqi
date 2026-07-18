// SPDX-License-Identifier: AGPL-3.0-or-later

// Package apikey implements a Bearer-token auth.Authenticator: it reads an
// Authorization: Bearer <key> header, resolves the key's hash to a live,
// non-revoked, non-expired api_keys row and its owning user, and returns the
// corresponding auth.Principal. Issuance/revocation live in the REST handler;
// this package only reads (plus a throttled last_used_at write).
package apikey

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/store"
)

// bearerPrefix is the Authorization scheme this authenticator consumes.
const bearerPrefix = "Bearer "

// touchThreshold is the minimum age of the stored last_used_at before a
// successful authentication rewrites it. It trades exact "last used" accuracy
// for one DB write per key per minute at most, so a busy key does not write on
// every request.
const touchThreshold = time.Minute

// ErrNoCredential means the request carried no Bearer Authorization header, so
// the caller (or a Chain) should fall through to another authenticator.
var ErrNoCredential = errors.New("apikey: no credential")

// APIKeySource is the store surface the authenticator needs.
type APIKeySource interface {
	GetAPIKeyByTokenHash(ctx context.Context, tokenHash string, now time.Time) (store.APIKey, error)
	GetUser(ctx context.Context, id string) (store.User, error)
	TouchAPIKeyLastUsed(ctx context.Context, id string, now time.Time) error
}

// Authenticator resolves a Bearer API key to a Principal.
type Authenticator struct {
	src APIKeySource
	now func() time.Time
}

var _ auth.Authenticator = (*Authenticator)(nil)

// New returns an API-key Authenticator. A nil clock defaults to time.Now UTC.
func New(src APIKeySource, clock func() time.Time) *Authenticator {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Authenticator{src: src, now: clock}
}

// Authenticate implements auth.Authenticator.
func (a *Authenticator) Authenticate(r *http.Request) (auth.Principal, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, bearerPrefix) {
		return auth.Principal{}, ErrNoCredential
	}
	raw := strings.TrimSpace(strings.TrimPrefix(h, bearerPrefix))
	if raw == "" {
		return auth.Principal{}, ErrNoCredential
	}
	now := a.now()
	key, err := a.src.GetAPIKeyByTokenHash(r.Context(), password.HashToken(raw), now)
	if err != nil {
		return auth.Principal{}, errors.New("apikey: invalid, revoked, or expired key")
	}
	u, err := a.src.GetUser(r.Context(), key.UserID)
	if err != nil {
		return auth.Principal{}, errors.New("apikey: user not found")
	}
	if u.Disabled {
		return auth.Principal{}, errors.New("apikey: account disabled")
	}
	a.maybeTouch(r.Context(), key, now)
	return auth.Principal{
		Subject:     u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Roles:       []string{u.Role},
		Kind:        auth.KindAPIKey,
		Superuser:   false,
	}, nil
}

// maybeTouch updates last_used_at only when it is unset or older than
// touchThreshold, so a busy key writes at most once per interval. A write
// failure is intentionally ignored: it must never fail an otherwise valid
// authentication.
func (a *Authenticator) maybeTouch(ctx context.Context, key store.APIKey, now time.Time) {
	if key.LastUsedAt != nil && now.Sub(*key.LastUsedAt) < touchThreshold {
		return
	}
	_ = a.src.TouchAPIKeyLastUsed(ctx, key.ID, now) //nolint:errcheck // best-effort; must not fail auth
}
