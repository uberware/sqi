// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── LicensePoolStore ──────────────────────────────────────────────────────────

// CreateLicensePool inserts a new pool. Returns [store.ErrConflict] if a pool
// with the same name already exists.
func (s *Store) CreateLicensePool(_ context.Context, pool store.LicensePool) (store.LicensePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.licensePools {
		if existing.Name == pool.Name {
			return store.LicensePool{}, store.ErrConflict
		}
	}

	s.licensePools[pool.ID] = pool
	return pool, nil
}

// GetLicensePool returns the pool with the given ID, or [store.ErrNotFound].
func (s *Store) GetLicensePool(_ context.Context, id string) (store.LicensePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, ok := s.licensePools[id]
	if !ok {
		return store.LicensePool{}, store.ErrNotFound
	}
	return pool, nil
}

// ListLicensePools returns all pools ordered by name.
func (s *Store) ListLicensePools(_ context.Context) ([]store.LicensePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pools := make([]store.LicensePool, 0, len(s.licensePools))
	for _, pool := range s.licensePools {
		pools = append(pools, pool)
	}

	slices.SortStableFunc(pools, func(a, b store.LicensePool) int {
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

// UpdateLicensePool replaces the mutable fields of an existing pool and
// updates UpdatedAt. Returns [store.ErrNotFound] or [store.ErrConflict].
func (s *Store) UpdateLicensePool(_ context.Context, pool store.LicensePool) (store.LicensePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.licensePools[pool.ID]
	if !ok {
		return store.LicensePool{}, store.ErrNotFound
	}

	// Check name uniqueness, excluding self.
	if pool.Name != existing.Name {
		for _, p := range s.licensePools {
			if p.Name == pool.Name {
				return store.LicensePool{}, store.ErrConflict
			}
		}
	}

	pool.CreatedAt = existing.CreatedAt
	pool.UpdatedAt = time.Now()
	s.licensePools[pool.ID] = pool
	return pool, nil
}

// DeleteLicensePool removes a pool by ID. Returns [store.ErrNotFound] if it
// does not exist.
func (s *Store) DeleteLicensePool(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.licensePools[id]; !ok {
		return store.ErrNotFound
	}

	delete(s.licensePools, id)
	return nil
}

// ── LicenseCheckoutStore ──────────────────────────────────────────────────────

// CreateCheckout inserts a new active checkout for the given pool and task
// attempt. The (TaskAttemptID, PoolID) pair must be unique; returns
// [store.ErrConflict] if violated.
func (s *Store) CreateCheckout(_ context.Context, checkout store.LicenseCheckout) (store.LicenseCheckout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.licenseCheckouts {
		if existing.TaskAttemptID == checkout.TaskAttemptID &&
			existing.PoolID == checkout.PoolID &&
			existing.ReleasedAt == nil {
			return store.LicenseCheckout{}, store.ErrConflict
		}
	}

	s.licenseCheckouts[checkout.ID] = checkout
	return checkout, nil
}

// ReleaseCheckout sets ReleasedAt on the checkout with the given ID, marking
// it as no longer active. Returns [store.ErrNotFound] if it does not exist.
func (s *Store) ReleaseCheckout(_ context.Context, id string, releasedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	checkout, ok := s.licenseCheckouts[id]
	if !ok {
		return store.ErrNotFound
	}

	checkout.ReleasedAt = &releasedAt
	s.licenseCheckouts[id] = checkout
	return nil
}

// ActiveCheckoutCount returns the number of checkouts for the given pool
// where ReleasedAt IS NULL.
func (s *Store) ActiveCheckoutCount(_ context.Context, poolID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, checkout := range s.licenseCheckouts {
		if checkout.PoolID == poolID && checkout.ReleasedAt == nil {
			count++
		}
	}
	return count, nil
}
