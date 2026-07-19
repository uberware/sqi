// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// Session is a server-side login session. Token is never stored; only its hash.
type Session struct {
	ID        string
	TokenHash string
	UserID    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// SessionStore is the persistence interface for [Session] records.
type SessionStore interface {
	// CreateSession inserts a new session.
	CreateSession(ctx context.Context, s Session) (Session, error)
	// GetSessionByTokenHash returns the session with the given token hash, or
	// [ErrNotFound] if missing or expired.
	GetSessionByTokenHash(ctx context.Context, tokenHash string, now time.Time) (Session, error)
	// GetSessionUserByTokenHash returns the user owning the unexpired session
	// with the given token hash, or [ErrNotFound] if no such session exists.
	//
	// It exists so authenticating a request costs one query rather than a
	// session lookup followed by a user lookup — this runs on every
	// cookie-authenticated request, so the second round trip is pure overhead
	// on the hottest path in the server.
	GetSessionUserByTokenHash(ctx context.Context, tokenHash string, now time.Time) (User, error)
	// DeleteSession removes a session by ID. Returns [ErrNotFound].
	DeleteSession(ctx context.Context, id string) error
	// DeleteSessionsForUser removes all sessions for a user.
	DeleteSessionsForUser(ctx context.Context, userID string) error
	// DeleteExpiredSessions removes sessions whose expiry is at or before now,
	// returning the count removed.
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)
}
