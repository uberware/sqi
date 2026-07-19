// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session implements a cookie-backed auth.Authenticator: it reads the
// session cookie, resolves its hashed token to a live session and user, and
// returns the corresponding auth.Principal. Issuance/revocation live in the
// auth REST handler; this package only reads.
package session

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/store"
)

// DefaultCookieName is the session cookie name.
const DefaultCookieName = "sqi_session"

// ErrNoCredential means the request carried no session cookie.
var ErrNoCredential = errors.New("session: no credential")

// SessionSource is the store surface the authenticator needs.
type SessionSource interface {
	GetSessionByTokenHash(ctx context.Context, tokenHash string, now time.Time) (store.Session, error)
	GetUser(ctx context.Context, id string) (store.User, error)
}

// Authenticator resolves a session cookie to a Principal.
type Authenticator struct {
	src        SessionSource
	cookieName string
	now        func() time.Time
}

var _ auth.Authenticator = (*Authenticator)(nil)

// New returns a session Authenticator. A nil clock defaults to time.Now.
func New(src SessionSource, cookieName string, clock func() time.Time) *Authenticator {
	if cookieName == "" {
		cookieName = DefaultCookieName
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Authenticator{src: src, cookieName: cookieName, now: clock}
}

// Authenticate implements auth.Authenticator.
func (a *Authenticator) Authenticate(r *http.Request) (auth.Principal, error) {
	c, err := r.Cookie(a.cookieName)
	if err != nil || c.Value == "" {
		return auth.Principal{}, ErrNoCredential
	}
	now := a.now()
	sess, err := a.src.GetSessionByTokenHash(r.Context(), password.HashToken(c.Value), now)
	if err != nil {
		return auth.Principal{}, errors.New("session: invalid or expired session")
	}
	u, err := a.src.GetUser(r.Context(), sess.UserID)
	if err != nil {
		return auth.Principal{}, errors.New("session: user not found")
	}
	if u.Disabled {
		return auth.Principal{}, errors.New("session: account disabled")
	}
	return auth.Principal{
		Subject:     u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Roles:       []string{u.Role},
		Kind:        auth.KindUser,
		Superuser:   false,
	}, nil
}
