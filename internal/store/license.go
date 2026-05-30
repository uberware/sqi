// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"time"
)

// LicensePool represents a tracked pool of concurrent software licenses.
// sqi enforces concurrency limits itself by counting active [LicenseCheckout]
// records; it does not query the upstream license server directly.
//
// ServerHint is informational only — the address of the vendor's license
// server, useful for diagnostics and future optional FLEXlm/RLM integration.
type LicensePool struct {
	ID            string
	Name          string
	Product       string
	ServerHint    string // e.g. "10.0.0.50:5053"; empty = not configured
	MaxConcurrent int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// LicenseCheckout records one active or completed checkout of a [LicensePool]
// slot by a specific [TaskAttempt]. ReleasedAt is nil while the checkout is
// active; it is set when the task attempt reaches a terminal state.
type LicenseCheckout struct {
	ID            string
	PoolID        string
	TaskAttemptID string
	CheckedOutAt  time.Time
	ReleasedAt    *time.Time // nil = currently active
}

// LicensePoolStore is the persistence interface for [LicensePool] records.
type LicensePoolStore interface {
	// CreateLicensePool inserts a new pool. Returns [ErrConflict] if a pool
	// with the same name already exists.
	CreateLicensePool(ctx context.Context, pool LicensePool) (LicensePool, error)

	// GetLicensePool returns the pool with the given ID, or [ErrNotFound].
	GetLicensePool(ctx context.Context, id string) (LicensePool, error)

	// ListLicensePools returns all pools ordered by name.
	ListLicensePools(ctx context.Context) ([]LicensePool, error)

	// UpdateLicensePool replaces the mutable fields of an existing pool and
	// updates UpdatedAt. Returns [ErrNotFound] or [ErrConflict].
	UpdateLicensePool(ctx context.Context, pool LicensePool) (LicensePool, error)

	// DeleteLicensePool removes a pool by ID. Returns [ErrNotFound] if it
	// does not exist.
	DeleteLicensePool(ctx context.Context, id string) error
}

// LicenseCheckoutStore is the persistence interface for [LicenseCheckout]
// records. It is separated from [LicensePoolStore] because the scheduler
// (task 52) is the primary caller and only needs checkout operations, not pool
// CRUD.
type LicenseCheckoutStore interface {
	// CreateCheckout inserts a new active checkout for the given pool and task
	// attempt. The (TaskAttemptID, PoolID) pair must be unique; returns
	// [ErrConflict] if violated.
	CreateCheckout(ctx context.Context, checkout LicenseCheckout) (LicenseCheckout, error)

	// ReleaseCheckout sets ReleasedAt on the checkout with the given ID,
	// marking it as no longer active. Returns [ErrNotFound] if it does not
	// exist.
	ReleaseCheckout(ctx context.Context, id string, releasedAt time.Time) error

	// ActiveCheckoutCount returns the number of checkouts for the given pool
	// where ReleasedAt IS NULL. Used by the scheduler's admission check before
	// assigning a task that requires the pool.
	ActiveCheckoutCount(ctx context.Context, poolID string) (int, error)
}
