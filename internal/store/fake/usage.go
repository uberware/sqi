// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── UsagePoolStore ────────────────────────────────────────────────────────────

// CreateUsagePool inserts a new pool. Returns [store.ErrConflict] if a pool with
// the same name (compared case-insensitively) already exists.
func (s *Store) CreateUsagePool(_ context.Context, pool store.UsagePool) (store.UsagePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Pool names are case-insensitive (OpenJD jobtemplate-2023-09), mirroring the
	// SQLite NOCASE unique index.
	for _, existing := range s.usagePools {
		if strings.EqualFold(existing.Name, pool.Name) {
			return store.UsagePool{}, store.ErrConflict
		}
	}

	s.usagePools[pool.ID] = pool
	return pool, nil
}

// GetUsagePool returns the pool with the given ID, or [store.ErrNotFound].
func (s *Store) GetUsagePool(_ context.Context, id string) (store.UsagePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.usagePools[id]
	if !ok {
		return store.UsagePool{}, store.ErrNotFound
	}
	return pool, nil
}

// ListUsagePools returns all pools ordered by name.
func (s *Store) ListUsagePools(_ context.Context) ([]store.UsagePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pools := make([]store.UsagePool, 0, len(s.usagePools))
	for _, pool := range s.usagePools {
		pools = append(pools, pool)
	}

	slices.SortStableFunc(pools, func(a, b store.UsagePool) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	return pools, nil
}

// ListUsagePoolUtilization returns all pools ordered by name, each paired with its
// current number of active claims.
func (s *Store) ListUsagePoolUtilization(_ context.Context) ([]store.UsagePoolUtilization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Count active claims per pool.
	active := make(map[string]int, len(s.usagePools))
	for _, co := range s.usageClaims {
		if co.ReleasedAt == nil {
			active[co.PoolID]++
		}
	}

	usage := make([]store.UsagePoolUtilization, 0, len(s.usagePools))
	for _, pool := range s.usagePools {
		usage = append(usage, store.UsagePoolUtilization{UsagePool: pool, InUse: active[pool.ID]})
	}

	slices.SortStableFunc(usage, func(a, b store.UsagePoolUtilization) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	return usage, nil
}

// UpdateUsagePool replaces the mutable fields of an existing pool and
// updates UpdatedAt. Returns [store.ErrNotFound] or [store.ErrConflict].
func (s *Store) UpdateUsagePool(_ context.Context, pool store.UsagePool) (store.UsagePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.usagePools[pool.ID]
	if !ok {
		return store.UsagePool{}, store.ErrNotFound
	}

	// Check name uniqueness case-insensitively, excluding self by ID (a pool may
	// rename itself between cases). Mirrors the SQLite NOCASE unique index.
	if !strings.EqualFold(pool.Name, existing.Name) {
		for id, p := range s.usagePools {
			if id != pool.ID && strings.EqualFold(p.Name, pool.Name) {
				return store.UsagePool{}, store.ErrConflict
			}
		}
	}

	pool.CreatedAt = existing.CreatedAt
	pool.UpdatedAt = time.Now()
	s.usagePools[pool.ID] = pool
	return pool, nil
}

// DeleteUsagePool removes a pool by ID. Returns [store.ErrNotFound] if it
// does not exist.
func (s *Store) DeleteUsagePool(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.usagePools[id]; !ok {
		return store.ErrNotFound
	}

	delete(s.usagePools, id)
	return nil
}

// ── UsageClaimStore ───────────────────────────────────────────────────────────

// ActiveClaimCount returns the number of claims for the given pool
// where ReleasedAt IS NULL.
func (s *Store) ActiveClaimCount(_ context.Context, poolID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.activeClaimsLocked(poolID), nil
}

// activeClaimsLocked counts poolID's active (unreleased) claims. Caller holds
// s.mu.
func (s *Store) activeClaimsLocked(poolID string) int {
	n := 0
	for _, claim := range s.usageClaims {
		if claim.PoolID == poolID && claim.ReleasedAt == nil {
			n++
		}
	}
	return n
}
