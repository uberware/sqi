// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"time"
)

// ErrUsageAtCapacity is returned by [UsageClaimStore.TryClaimSlots]
// when one or more required usage pools are saturated (active claims have
// reached MaxConcurrent). The task should remain ready for reassignment on the
// next scheduler tick.
var ErrUsageAtCapacity = errors.New("store: usage pool at capacity")

// UsagePoolClaim describes a single usage pool slot to be claimed
// atomically by [UsageClaimStore.TryClaimSlots].
type UsagePoolClaim struct {
	// ClaimID is a caller-supplied UUID for the claim row to be created.
	ClaimID string
	// PoolID is the [UsagePool] to claim from.
	PoolID string
	// PoolName is used in error messages and debug logging only.
	PoolName string
	// MaxConcurrent is the pool's current limit. The implementation uses this
	// value inside the transaction to avoid a second pool lookup.
	MaxConcurrent int
}

// UsagePool represents a tracked named concurrency limit.
// sqi enforces concurrency limits itself by counting active [UsageClaim]
// records; it does not query any upstream server directly.
//
// ServerHint is informational only — the address of a vendor's software
// server (FLEXlm/RLM), useful for diagnostics when the pool represents
// software slots. Leave empty for non-vendor dimensions.
type UsagePool struct {
	ID            string
	Name          string
	ServerHint    string // e.g. "10.0.0.50:5053"; empty = not configured
	MaxConcurrent int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// UsagePoolUtilization pairs a [UsagePool] with its current live utilization —
// the number of active (unreleased) claims against it. It is returned by
// [UsagePoolStore.ListUsagePoolUtilization] so callers can render "in use vs
// capacity" without an N+1 fan-out of per-pool count queries.
type UsagePoolUtilization struct {
	UsagePool

	// InUse is the number of active claims (released_at IS NULL) for the pool.
	InUse int
}

// UsageClaim records one active or completed claim of a [UsagePool]
// slot by a specific [TaskAttempt]. ReleasedAt is nil while the claim is
// active; it is set when the task attempt reaches a terminal state.
type UsageClaim struct {
	ID            string
	PoolID        string
	TaskAttemptID string
	// DB column: checked_out_at (historic name; kept to avoid a column-rename migration).
	ClaimedAt  time.Time
	ReleasedAt *time.Time // nil = currently active
}

// UsagePoolStore is the persistence interface for [UsagePool] records.
type UsagePoolStore interface {
	// CreateUsagePool inserts a new pool. Returns [ErrConflict] if a pool
	// with the same name already exists.
	CreateUsagePool(ctx context.Context, pool UsagePool) (UsagePool, error)

	// GetUsagePool returns the pool with the given ID, or [ErrNotFound].
	GetUsagePool(ctx context.Context, id string) (UsagePool, error)

	// ListUsagePools returns all pools ordered by name.
	ListUsagePools(ctx context.Context) ([]UsagePool, error)

	// ListUsagePoolUtilization returns all pools ordered by name, each paired with
	// its current number of active claims. It computes utilization in a
	// single query rather than one count per pool.
	ListUsagePoolUtilization(ctx context.Context) ([]UsagePoolUtilization, error)

	// UpdateUsagePool replaces the mutable fields of an existing pool and
	// updates UpdatedAt. Returns [ErrNotFound] or [ErrConflict].
	UpdateUsagePool(ctx context.Context, pool UsagePool) (UsagePool, error)

	// DeleteUsagePool removes a pool by ID. Returns [ErrNotFound] if it
	// does not exist.
	DeleteUsagePool(ctx context.Context, id string) error
}

// UsageClaimStore is the persistence interface for [UsageClaim]
// records. It is separated from [UsagePoolStore] because the scheduler
// is the primary caller and only needs claim operations, not pool
// CRUD.
type UsageClaimStore interface {
	// CreateClaim inserts a new active claim for the given pool and task
	// attempt. The (TaskAttemptID, PoolID) pair must be unique; returns
	// [ErrConflict] if violated.
	CreateClaim(ctx context.Context, claim UsageClaim) (UsageClaim, error)

	// ReleaseClaim sets ReleasedAt on the claim with the given ID,
	// marking it as no longer active. Returns [ErrNotFound] if it does not
	// exist.
	ReleaseClaim(ctx context.Context, id string, releasedAt time.Time) error

	// ActiveClaimCount returns the number of claims for the given pool
	// where ReleasedAt IS NULL. Used by the scheduler's admission check before
	// assigning a task that requires the pool.
	ActiveClaimCount(ctx context.Context, poolID string) (int, error)

	// TryClaimSlots atomically verifies that every pool in claims has
	// remaining capacity and, if so, inserts a [UsageClaim] row for each.
	// The operation runs inside a single database transaction so the
	// count-check and insert are indivisible.
	//
	// Returns [ErrUsageAtCapacity] if any pool is saturated; no rows are
	// inserted in that case. Returns [ErrConflict] if any claim ID or
	// (task_attempt_id, pool_id) pair already exists.
	//
	// taskAttemptID must reference an existing [TaskAttempt] row. claimedAt
	// is the timestamp recorded on each new claim.
	TryClaimSlots(ctx context.Context, taskAttemptID string, claims []UsagePoolClaim, claimedAt time.Time) error

	// ReleaseAttemptClaims sets ReleasedAt on every active claim
	// (released_at IS NULL) for the given taskAttemptID. Called when a task
	// attempt reaches a terminal state so the usage pool slots are freed for
	// reassignment.
	//
	// Returns the number of claims released (0 is not an error when the
	// attempt held no claims).
	ReleaseAttemptClaims(ctx context.Context, taskAttemptID string, releasedAt time.Time) (int, error)

	// ReleaseJobClaims sets ReleasedAt on every active claim for any task
	// attempt belonging to a task in the given job. Called during job
	// cancellation to free all usage pool slots held by that job in a single
	// operation, rather than iterating through individual attempts.
	//
	// Returns the number of claims released (0 is not an error when the job
	// held no claims).
	ReleaseJobClaims(ctx context.Context, jobID string, releasedAt time.Time) (int, error)
}
