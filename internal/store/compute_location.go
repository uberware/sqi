// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// ComputeLocation is a named, globally-registered compute location. Workers
// declare their location at registration; jobs/steps target a location via the
// native OpenJD attr.worker.computelocation host requirement. The registry is a
// curated catalog used for surfacing and management — it does not gate
// scheduling (the matcher keys on the raw string).
//
// Names are unique case-insensitively, matching the matcher's case-insensitive
// comparison and the keys used in [StorageLocation.Roots].
type ComputeLocation struct {
	ID          string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ComputeLocationStore is the persistence interface for [ComputeLocation] records.
type ComputeLocationStore interface {
	// CreateComputeLocation inserts a new location. Returns [ErrConflict] if a
	// location with the same name (case-insensitive) already exists.
	CreateComputeLocation(ctx context.Context, loc ComputeLocation) (ComputeLocation, error)

	// GetComputeLocation returns the location with the given ID, or [ErrNotFound].
	GetComputeLocation(ctx context.Context, id string) (ComputeLocation, error)

	// GetComputeLocationByName returns the location with the given name
	// (case-insensitive), or [ErrNotFound].
	GetComputeLocationByName(ctx context.Context, name string) (ComputeLocation, error)

	// ListComputeLocations returns all locations ordered by name.
	ListComputeLocations(ctx context.Context) ([]ComputeLocation, error)

	// UpdateComputeLocation replaces the mutable fields of an existing location
	// and updates UpdatedAt. Returns [ErrNotFound] or [ErrConflict].
	UpdateComputeLocation(ctx context.Context, loc ComputeLocation) (ComputeLocation, error)

	// DeleteComputeLocation removes a location by ID. Returns [ErrNotFound] if
	// it does not exist.
	DeleteComputeLocation(ctx context.Context, id string) error
}
