// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"time"
)

// StorageLocationType identifies the backing storage technology.
type StorageLocationType string

const (
	// StorageLocationTypeFilesystem is a POSIX or Windows filesystem path.
	StorageLocationTypeFilesystem StorageLocationType = "filesystem"
	// StorageLocationTypeS3 is an S3-compatible object store (AWS S3,
	// Backblaze B2, Cloudflare R2, MinIO, etc.).
	StorageLocationTypeS3 StorageLocationType = "s3"
)

// StorageLocation is a named, globally-registered storage root. Jobs reference
// locations by name with a relative path; workers resolve the name to a
// concrete path for their compute location at execution time.
//
// Roots maps a compute location name (or "default") to the root path for that
// location. Example:
//
//	Roots = map[string]string{
//	    "default":         "/mnt/nas/shows",
//	    "windows_workers": `Z:\shows`,
//	    "cloud_aws":       "s3://studio-bucket/shows",
//	}
type StorageLocation struct {
	ID          string
	Name        string
	Type        StorageLocationType
	Description string
	Roots       map[string]string // compute_location_name -> root path
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// StorageLocationStore is the persistence interface for [StorageLocation] records.
type StorageLocationStore interface {
	// CreateStorageLocation inserts a new location. Returns [ErrConflict] if
	// a location with the same name already exists.
	CreateStorageLocation(ctx context.Context, loc StorageLocation) (StorageLocation, error)

	// GetStorageLocation returns the location with the given ID, or [ErrNotFound].
	GetStorageLocation(ctx context.Context, id string) (StorageLocation, error)

	// GetStorageLocationByName returns the location with the given name, or
	// [ErrNotFound]. Used by the path resolver to look up locations by their
	// symbolic name at task dispatch time.
	GetStorageLocationByName(ctx context.Context, name string) (StorageLocation, error)

	// ListStorageLocations returns all locations ordered by name.
	ListStorageLocations(ctx context.Context) ([]StorageLocation, error)

	// UpdateStorageLocation replaces the mutable fields of an existing
	// location and updates UpdatedAt. Returns [ErrNotFound] or [ErrConflict].
	UpdateStorageLocation(ctx context.Context, loc StorageLocation) (StorageLocation, error)

	// DeleteStorageLocation removes a location by ID. Returns [ErrNotFound]
	// if it does not exist.
	DeleteStorageLocation(ctx context.Context, id string) error
}
