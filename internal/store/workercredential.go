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
	// CapacityGroupID is the capacity group the credential enrolled into with a
	// group join token, or "" for an ordinary credential. Like WorkerID it is
	// not a foreign key: the credential outlives a deleted group.
	CapacityGroupID string
}

// WorkerJoinToken is a time-limited token an operator issues so a worker can
// enroll and receive a [WorkerCredential]. Only TokenHash is stored; the raw
// token is shown to the operator exactly once, at creation.
type WorkerJoinToken struct {
	ID        string
	TokenHash string // SHA-256 of the raw token; Go-side only
	Prefix    string // leading chars of the raw token, for list identification
	Name      string
	ExpiresAt time.Time
	UsedAt    *time.Time // nil = not yet redeemed
	CreatedBy string
	CreatedAt time.Time
	// CapacityGroupID binds the token to a capacity group: every worker that
	// enrolls with it joins that group, and the token can be redeemed any
	// number of times. "" is an ordinary token. A token goes with its group.
	CapacityGroupID string
	// RevokedAt is when an operator revoked the token; nil = not revoked. A
	// revoked token enrolls nobody.
	RevokedAt *time.Time
}

// WorkerCredentialStore is the persistence interface for [WorkerCredential]
// and [WorkerJoinToken] records.
type WorkerCredentialStore interface {
	// CreateWorkerCredential inserts a new credential. Returns [ErrConflict]
	// on a worker-id, public-key, or id collision.
	CreateWorkerCredential(ctx context.Context, c WorkerCredential) (WorkerCredential, error)
	// ListActiveWorkerCredentials returns every credential with a nil
	// RevokedAt, the set "sqi-server worker list" shows. A revoked credential
	// must never appear in the result.
	ListActiveWorkerCredentials(ctx context.Context) ([]WorkerCredential, error)
	// GetActiveWorkerCredentialByPublicKey returns the credential whose
	// PublicKey is publicKey and whose RevokedAt is nil, or [ErrNotFound] if
	// no active credential has that key. A revoked key and an unknown key
	// both return [ErrNotFound]: either means "do not authenticate".
	GetActiveWorkerCredentialByPublicKey(ctx context.Context, publicKey string) (WorkerCredential, error)
	// RevokeWorkerCredential soft-revokes the credential for workerID.
	// Returns [ErrNotFound] if no such worker has a credential.
	RevokeWorkerCredential(ctx context.Context, workerID string, at time.Time) error
	// TouchWorkerCredential sets LastSeenAt on workerID's ACTIVE credential.
	// Returns [ErrNotFound] if the worker has no active credential. Callers
	// that treat "seen" as best-effort bookkeeping (registration, not a
	// security decision) should not fail on that error.
	TouchWorkerCredential(ctx context.Context, workerID string, at time.Time) error
	// CreateWorkerJoinToken inserts a new join token. Returns [ErrConflict]
	// on a token-hash or id collision.
	CreateWorkerJoinToken(ctx context.Context, t WorkerJoinToken) (WorkerJoinToken, error)
	// GetWorkerJoinTokenByHash returns the token for hash, or [ErrNotFound] if
	// no such token exists.
	GetWorkerJoinTokenByHash(ctx context.Context, hash string) (WorkerJoinToken, error)
	// MarkWorkerJoinTokenUsed sets UsedAt. Returns [ErrNotFound] if id is
	// unknown.
	MarkWorkerJoinTokenUsed(ctx context.Context, id string, at time.Time) error
	// ListCapacityGroupTokens returns every join token bound to the group,
	// revoked and expired ones included, newest first. An unknown group has
	// none.
	ListCapacityGroupTokens(ctx context.Context, groupID string) ([]WorkerJoinToken, error)
	// RevokeWorkerJoinToken marks the token revoked at the given time.
	// Returns [ErrNotFound] if id is unknown; revoking a revoked token is not
	// an error and keeps the first revocation time.
	RevokeWorkerJoinToken(ctx context.Context, id string, at time.Time) error
	// RedeemWorkerJoinToken atomically claims the single-use join token
	// identified by hash AND creates cred, in one transaction. It succeeds
	// only for a token that exists, has not been redeemed or revoked, and has
	// not expired at now; every other case — including a token another request
	// claimed a moment earlier — returns [ErrNotFound], and cred is not
	// created. Returns [ErrConflict] if cred cannot be created (a worker ID
	// already bound to an active credential, or a public key already
	// enrolled to any worker); in that case the whole transaction rolls
	// back, so the token is NOT consumed and remains redeemable.
	//
	// The token claim and the credential creation must be ONE transaction.
	// Committing the claim before the credential is known to be creatable
	// would burn a single-use token on a request that was always going to
	// be rejected as a conflict — the caller gets nothing for it, and the
	// operator has to issue a new one. Within the claim itself, check and
	// set must still be a single statement: reading the token, inspecting
	// UsedAt and marking it used separately is a check-then-act race, where
	// two concurrent enrollments presenting the same single-use token both
	// observe UsedAt as nil and both succeed.
	RedeemWorkerJoinToken(ctx context.Context, hash string, now time.Time, cred WorkerCredential) (WorkerCredential, error)
}
