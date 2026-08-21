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
	// GetActiveWorkerCredentialByWorkerID returns the ACTIVE credential for
	// workerID — the one with a nil RevokedAt — or [ErrNotFound] if the
	// worker has none. A revoked credential is never retrievable through
	// this method, even if it is the only credential the worker has ever
	// had: after a key rotation (revoke, then re-enroll with a new key) a
	// worker can have both a revoked row and an active one, and this method
	// exists specifically to resolve that ambiguity to the one row that
	// matters for authentication.
	GetActiveWorkerCredentialByWorkerID(ctx context.Context, workerID string) (WorkerCredential, error)
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
	// ConsumeWorkerJoinToken atomically claims the single-use join token
	// identified by hash, returning it with UsedAt set to now. It succeeds
	// only for a token that exists, has not been redeemed, and has not
	// expired at now; every other case — including a token another request
	// claimed a moment earlier — returns [ErrNotFound].
	//
	// Check and claim must be ONE statement. Reading the token, inspecting
	// UsedAt and marking it used separately is a check-then-act race: two
	// concurrent enrollments presenting the same single-use token both
	// observe UsedAt as nil and both succeed, and a failure of the later
	// write leaves the token redeemable until it expires while the caller
	// was already told it succeeded.
	ConsumeWorkerJoinToken(ctx context.Context, hash string, now time.Time) (WorkerJoinToken, error)
}
