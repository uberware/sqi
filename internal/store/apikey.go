// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// APIKey is a long-lived machine credential owned by a user. TokenHash is a
// Go-side field only; it is never marshaled into a REST response, and the raw
// key it hashes is shown to the client exactly once, at creation.
type APIKey struct {
	ID         string
	UserID     string
	Name       string
	Prefix     string     // leading chars of the raw key, for list identification
	TokenHash  string     // SHA-256 of the raw key; Go-side only
	ExpiresAt  *time.Time // nil = never expires
	LastUsedAt *time.Time // nil = never used
	RevokedAt  *time.Time // nil = active
	CreatedAt  time.Time
}

// APIKeyStore is the persistence interface for [APIKey] records.
type APIKeyStore interface {
	// CreateAPIKey inserts a new key. Returns [ErrConflict] on a token-hash or
	// id collision.
	CreateAPIKey(ctx context.Context, k APIKey) (APIKey, error)
	// GetAPIKeyByTokenHash returns the active key for tokenHash, or
	// [ErrNotFound] if it is missing, revoked, or expired at now.
	GetAPIKeyByTokenHash(ctx context.Context, tokenHash string, now time.Time) (APIKey, error)
	// ListAPIKeysForUser returns the user's non-revoked keys, newest first.
	ListAPIKeysForUser(ctx context.Context, userID string) ([]APIKey, error)
	// RevokeAPIKey soft-revokes the key, scoped to its owner. Returns
	// [ErrNotFound] if id is not one of userID's keys.
	RevokeAPIKey(ctx context.Context, id, userID string, now time.Time) error
	// TouchAPIKeyLastUsed sets last_used_at. Returns [ErrNotFound].
	TouchAPIKeyLastUsed(ctx context.Context, id string, now time.Time) error
}
