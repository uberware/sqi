// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"time"
)

// Farm is the top-level organizational grouping in sqi. A deployment typically
// has one farm, but large facilities may define multiple to represent distinct
// infrastructure pools, studios, or departments.
type Farm struct {
	ID          string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// FarmStore is the persistence interface for [Farm] records.
type FarmStore interface {
	// CreateFarm inserts a new farm. The caller must populate all fields;
	// the implementation persists them as-is. Returns [ErrConflict] if a
	// farm with the same name already exists.
	CreateFarm(ctx context.Context, farm Farm) (Farm, error)

	// GetFarm returns the farm with the given ID, or [ErrNotFound].
	GetFarm(ctx context.Context, id string) (Farm, error)

	// ListFarms returns all farms ordered by name.
	ListFarms(ctx context.Context) ([]Farm, error)

	// UpdateFarm replaces the mutable fields (Name, Description) of an
	// existing farm and updates UpdatedAt. Returns [ErrNotFound] if the farm
	// does not exist, or [ErrConflict] if the new name is already taken.
	UpdateFarm(ctx context.Context, farm Farm) (Farm, error)

	// DeleteFarm removes a farm by ID. Returns [ErrNotFound] if it does not
	// exist. The caller is responsible for ensuring no queues or workers
	// reference the farm before deleting it.
	DeleteFarm(ctx context.Context, id string) error
}
