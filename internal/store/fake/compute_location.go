// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateComputeLocation inserts a new location. Returns [store.ErrConflict] if a
// location with the same name already exists.
func (s *Store) CreateComputeLocation(_ context.Context, loc store.ComputeLocation) (store.ComputeLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.computeLocations {
		if existing.Name == loc.Name {
			return store.ComputeLocation{}, store.ErrConflict
		}
	}
	s.computeLocations[loc.ID] = loc
	return loc, nil
}

// GetComputeLocation returns the location with the given ID, or [store.ErrNotFound].
func (s *Store) GetComputeLocation(_ context.Context, id string) (store.ComputeLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	loc, ok := s.computeLocations[id]
	if !ok {
		return store.ComputeLocation{}, store.ErrNotFound
	}
	return loc, nil
}

// GetComputeLocationByName returns the location with the given name, or
// [store.ErrNotFound]. The match is exact (case-sensitive).
func (s *Store) GetComputeLocationByName(_ context.Context, name string) (store.ComputeLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, loc := range s.computeLocations {
		if loc.Name == name {
			return loc, nil
		}
	}
	return store.ComputeLocation{}, store.ErrNotFound
}

// ListComputeLocations returns all locations ordered by name.
func (s *Store) ListComputeLocations(_ context.Context) ([]store.ComputeLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	locs := make([]store.ComputeLocation, 0, len(s.computeLocations))
	for _, loc := range s.computeLocations {
		locs = append(locs, loc)
	}
	slices.SortStableFunc(locs, func(a, b store.ComputeLocation) int {
		return strings.Compare(a.Name, b.Name)
	})
	return locs, nil
}

// UpdateComputeLocation replaces the mutable fields of an existing location and
// updates UpdatedAt. Returns [store.ErrNotFound] or [store.ErrConflict].
func (s *Store) UpdateComputeLocation(_ context.Context, loc store.ComputeLocation) (store.ComputeLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.computeLocations[loc.ID]
	if !ok {
		return store.ComputeLocation{}, store.ErrNotFound
	}
	if loc.Name != existing.Name {
		for _, l := range s.computeLocations {
			if l.Name == loc.Name {
				return store.ComputeLocation{}, store.ErrConflict
			}
		}
	}
	loc.CreatedAt = existing.CreatedAt
	loc.UpdatedAt = time.Now().UTC()
	s.computeLocations[loc.ID] = loc
	return loc, nil
}

// DeleteComputeLocation removes a location by ID. Returns [store.ErrNotFound] if
// it does not exist.
func (s *Store) DeleteComputeLocation(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.computeLocations[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.computeLocations, id)
	return nil
}
