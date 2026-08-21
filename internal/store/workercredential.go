// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// WorkerCredential binds a worker's self-chosen ID to an Ed25519 nkey public
// key, issued during enrollment and presented on every broker connection
// thereafter. WorkerID is deliberately not a foreign key to a worker row: a
// credential is issued before the worker has ever registered, and on
// auth-off farms worker rows exist with no credential at all.
type WorkerCredential struct {
	ID         string
	WorkerID   string
	PublicKey  string
	Name       string
	EnrolledAt time.Time
	LastSeenAt *time.Time // nil = never seen since enrollment
	RevokedAt  *time.Time // nil = active
}

// WorkerJoinToken is a single-use, time-limited token an operator issues so a
// worker can enroll and receive a [WorkerCredential]. Only TokenHash is
// stored; the raw token is shown to the operator exactly once, at creation.
type WorkerJoinToken struct {
	ID        string
	TokenHash string // SHA-256 of the raw token; Go-side only
	Prefix    string // leading chars of the raw token, for list identification
	Name      string
	ExpiresAt time.Time
	UsedAt    *time.Time // nil = not yet redeemed
	CreatedBy string
	CreatedAt time.Time
}

// WorkerCredentialStore is the persistence interface for [WorkerCredential]
// and [WorkerJoinToken] records.
type WorkerCredentialStore interface {
	// CreateWorkerCredential inserts a new credential. Returns [ErrConflict]
	// on a worker-id, public-key, or id collision.
	CreateWorkerCredential(ctx context.Context, c WorkerCredential) (WorkerCredential, error)
	// GetWorkerCredentialByWorkerID returns the credential for workerID, or
	// [ErrNotFound] if none exists.
	GetWorkerCredentialByWorkerID(ctx context.Context, workerID string) (WorkerCredential, error)
	// ListActiveWorkerCredentials returns every credential with a nil
	// RevokedAt. It is what the broker's authorized-key set is rebuilt from,
	// so a revoked credential must never appear in the result.
	ListActiveWorkerCredentials(ctx context.Context) ([]WorkerCredential, error)
	// RevokeWorkerCredential soft-revokes the credential for workerID.
	// Returns [ErrNotFound] if no such worker has a credential.
	RevokeWorkerCredential(ctx context.Context, workerID string, at time.Time) error
	// CreateWorkerJoinToken inserts a new join token. Returns [ErrConflict]
	// on a token-hash or id collision.
	CreateWorkerJoinToken(ctx context.Context, t WorkerJoinToken) (WorkerJoinToken, error)
	// GetWorkerJoinTokenByHash returns the token for hash, or [ErrNotFound] if
	// no such token exists.
	GetWorkerJoinTokenByHash(ctx context.Context, hash string) (WorkerJoinToken, error)
	// MarkWorkerJoinTokenUsed sets UsedAt. Returns [ErrNotFound] if id is
	// unknown.
	MarkWorkerJoinTokenUsed(ctx context.Context, id string, at time.Time) error
}
