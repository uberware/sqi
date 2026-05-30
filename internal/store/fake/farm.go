// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateFarm inserts a new farm. Returns [store.ErrConflict] if a farm with
// the same name already exists.
func (s *Store) CreateFarm(_ context.Context, farm store.Farm) (store.Farm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.farms {
		if existing.Name == farm.Name {
			return store.Farm{}, store.ErrConflict
		}
	}

	s.farms[farm.ID] = farm
	return farm, nil
}

// GetFarm returns the farm with the given ID, or [store.ErrNotFound].
func (s *Store) GetFarm(_ context.Context, id string) (store.Farm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	farm, ok := s.farms[id]
	if !ok {
		return store.Farm{}, store.ErrNotFound
	}
	return farm, nil
}

// ListFarms returns all farms ordered by name.
func (s *Store) ListFarms(_ context.Context) ([]store.Farm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	farms := make([]store.Farm, 0, len(s.farms))
	for _, farm := range s.farms {
		farms = append(farms, farm)
	}

	slices.SortStableFunc(farms, func(a, b store.Farm) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	return farms, nil
}

// UpdateFarm replaces the mutable fields (Name, Description) of an existing
// farm and updates UpdatedAt. Returns [store.ErrNotFound] if the farm does
// not exist, or [store.ErrConflict] if the new name is already taken.
func (s *Store) UpdateFarm(_ context.Context, farm store.Farm) (store.Farm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.farms[farm.ID]
	if !ok {
		return store.Farm{}, store.ErrNotFound
	}

	// Check name uniqueness, excluding self.
	if farm.Name != existing.Name {
		for _, f := range s.farms {
			if f.Name == farm.Name {
				return store.Farm{}, store.ErrConflict
			}
		}
	}

	farm.CreatedAt = existing.CreatedAt
	farm.UpdatedAt = time.Now()
	s.farms[farm.ID] = farm
	return farm, nil
}

// DeleteFarm removes a farm by ID. Returns [store.ErrNotFound] if it does
// not exist.
func (s *Store) DeleteFarm(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.farms[id]; !ok {
		return store.ErrNotFound
	}

	delete(s.farms, id)
	return nil
}
