// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

// ── CapacityGroupStore ────────────────────────────────────────────────────────

// storedCapacityGroup copies g the way the SQL stores keep it: durations to
// the millisecond, empty collections as nil, and no slice or map shared with
// the caller.
func storedCapacityGroup(g store.CapacityGroup) store.CapacityGroup {
	g.ScaleUpCooldown = g.ScaleUpCooldown.Truncate(1e6)
	g.IdleTimeout = g.IdleTimeout.Truncate(1e6)
	g.BootTimeout = g.BootTimeout.Truncate(1e6)
	g.QueueIDs = nilIfEmpty(slices.Clone(g.QueueIDs))
	g.Profile.Extensions = nilIfEmpty(slices.Clone(g.Profile.Extensions))
	if len(g.Profile.Tags) == 0 {
		g.Profile.Tags = nil
	} else {
		g.Profile.Tags = maps.Clone(g.Profile.Tags)
	}
	return g
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// CreateCapacityGroup inserts a new group. Returns [store.ErrConflict] if the
// name is taken or the farm does not exist.
func (s *Store) CreateCapacityGroup(ctx context.Context, g store.CapacityGroup) (store.CapacityGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.CapacityGroup{}, err
	}

	if _, ok := s.farms[g.FarmID]; !ok {
		return store.CapacityGroup{}, store.ErrConflict
	}
	if _, ok := s.capacityGroups[g.ID]; ok {
		return store.CapacityGroup{}, store.ErrConflict
	}
	for _, existing := range s.capacityGroups {
		if existing.Name == g.Name {
			return store.CapacityGroup{}, store.ErrConflict
		}
	}

	g = storedCapacityGroup(g)
	g.CreatedAt = s.stampLocked()
	g.UpdatedAt = g.CreatedAt
	s.capacityGroups[g.ID] = g
	return storedCapacityGroup(g), nil
}

// GetCapacityGroup returns the group with the given ID, or [store.ErrNotFound].
func (s *Store) GetCapacityGroup(_ context.Context, id string) (store.CapacityGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.capacityGroups[id]
	if !ok {
		return store.CapacityGroup{}, store.ErrNotFound
	}
	return storedCapacityGroup(g), nil
}

// GetCapacityGroupByName returns the group with the given name, or
// [store.ErrNotFound].
func (s *Store) GetCapacityGroupByName(_ context.Context, name string) (store.CapacityGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, g := range s.capacityGroups {
		if g.Name == name {
			return storedCapacityGroup(g), nil
		}
	}
	return store.CapacityGroup{}, store.ErrNotFound
}

// ListCapacityGroups returns all groups ordered by priority descending, then
// name.
func (s *Store) ListCapacityGroups(_ context.Context) ([]store.CapacityGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	groups := make([]store.CapacityGroup, 0, len(s.capacityGroups))
	for _, g := range s.capacityGroups {
		groups = append(groups, storedCapacityGroup(g))
	}
	slices.SortFunc(groups, func(a, b store.CapacityGroup) int {
		if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return groups, nil
}

// UpdateCapacityGroup replaces the mutable fields of an existing group and
// updates UpdatedAt; the ID, farm and CreatedAt are kept. Returns
// [store.ErrNotFound] or [store.ErrConflict].
func (s *Store) UpdateCapacityGroup(ctx context.Context, g store.CapacityGroup) (store.CapacityGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.CapacityGroup{}, err
	}

	existing, ok := s.capacityGroups[g.ID]
	if !ok {
		return store.CapacityGroup{}, store.ErrNotFound
	}
	for id, other := range s.capacityGroups {
		if id != g.ID && other.Name == g.Name {
			return store.CapacityGroup{}, store.ErrConflict
		}
	}

	g = storedCapacityGroup(g)
	g.FarmID = existing.FarmID
	g.CreatedAt = existing.CreatedAt
	g.UpdatedAt = s.stampLocked()
	s.capacityGroups[g.ID] = g
	return storedCapacityGroup(g), nil
}

// DeleteCapacityGroup removes a group by ID. Returns [store.ErrNotFound] if it
// does not exist.
func (s *Store) DeleteCapacityGroup(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return err
	}

	if _, ok := s.capacityGroups[id]; !ok {
		return store.ErrNotFound
	}
	// A worker that belongs to the group makes the SQL stores refuse with
	// [store.ErrConflict]; workers carry no group yet, so nothing can.
	delete(s.capacityGroups, id)
	return nil
}
