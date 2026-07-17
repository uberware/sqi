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
	// DeleteSession removes a session by ID. Returns [ErrNotFound].
	DeleteSession(ctx context.Context, id string) error
	// DeleteSessionsForUser removes all sessions for a user.
	DeleteSessionsForUser(ctx context.Context, userID string) error
	// DeleteExpiredSessions removes sessions whose expiry is at or before now,
	// returning the count removed.
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)
}
