// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateStorageLocation inserts a new location. Returns [store.ErrConflict] if
// a location with the same name already exists.
func (s *Store) CreateStorageLocation(_ context.Context, loc store.StorageLocation) (store.StorageLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.storageLocations {
		if existing.Name == loc.Name {
			return store.StorageLocation{}, store.ErrConflict
		}
	}

	loc.Roots = copyMap(loc.Roots)
	s.storageLocations[loc.ID] = loc
	return loc, nil
}

// GetStorageLocation returns the location with the given ID, or [store.ErrNotFound].
func (s *Store) GetStorageLocation(_ context.Context, id string) (store.StorageLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	loc, ok := s.storageLocations[id]
	if !ok {
		return store.StorageLocation{}, store.ErrNotFound
	}

	loc.Roots = copyMap(loc.Roots)
	return loc, nil
}

// GetStorageLocationByName returns the location with the given name, or
// [store.ErrNotFound].
func (s *Store) GetStorageLocationByName(_ context.Context, name string) (store.StorageLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, loc := range s.storageLocations {
		if loc.Name == name {
			loc.Roots = copyMap(loc.Roots)
			return loc, nil
		}
	}

	return store.StorageLocation{}, store.ErrNotFound
}

// ListStorageLocations returns all locations ordered by name.
func (s *Store) ListStorageLocations(_ context.Context) ([]store.StorageLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	locs := make([]store.StorageLocation, 0, len(s.storageLocations))
	for _, loc := range s.storageLocations {
		loc.Roots = copyMap(loc.Roots)
		locs = append(locs, loc)
	}

	slices.SortStableFunc(locs, func(a, b store.StorageLocation) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	return locs, nil
}

// UpdateStorageLocation replaces the mutable fields of an existing location
// and updates UpdatedAt. Returns [store.ErrNotFound] or [store.ErrConflict].
func (s *Store) UpdateStorageLocation(_ context.Context, loc store.StorageLocation) (store.StorageLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.storageLocations[loc.ID]
	if !ok {
		return store.StorageLocation{}, store.ErrNotFound
	}

	// Check name uniqueness, excluding self.
	if loc.Name != existing.Name {
		for _, l := range s.storageLocations {
			if l.Name == loc.Name {
				return store.StorageLocation{}, store.ErrConflict
			}
		}
	}

	loc.CreatedAt = existing.CreatedAt
	loc.UpdatedAt = time.Now()
	loc.Roots = copyMap(loc.Roots)
	s.storageLocations[loc.ID] = loc
	return loc, nil
}

// DeleteStorageLocation removes a location by ID. Returns [store.ErrNotFound]
// if it does not exist.
func (s *Store) DeleteStorageLocation(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.storageLocations[id]; !ok {
		return store.ErrNotFound
	}

	delete(s.storageLocations, id)
	return nil
}
