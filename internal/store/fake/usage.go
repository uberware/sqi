// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── UsagePoolStore ────────────────────────────────────────────────────────────

// CreateUsagePool inserts a new pool. Returns [store.ErrConflict] if a pool
// with the same name already exists.
func (s *Store) CreateUsagePool(_ context.Context, pool store.UsagePool) (store.UsagePool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.usagePools {
		if existing.Name == pool.Name {
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

	// Check name uniqueness, excluding self.
	if pool.Name != existing.Name {
		for _, p := range s.usagePools {
			if p.Name == pool.Name {
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

// CreateClaim inserts a new active claim for the given pool and task
// attempt. The (TaskAttemptID, PoolID) pair must be unique; returns
// [store.ErrConflict] if violated.
func (s *Store) CreateClaim(_ context.Context, claim store.UsageClaim) (store.UsageClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.usageClaims {
		if existing.TaskAttemptID == claim.TaskAttemptID &&
			existing.PoolID == claim.PoolID &&
			existing.ReleasedAt == nil {
			return store.UsageClaim{}, store.ErrConflict
		}
	}

	s.usageClaims[claim.ID] = claim
	return claim, nil
}

// ReleaseClaim sets ReleasedAt on the claim with the given ID, marking
// it as no longer active. Returns [store.ErrNotFound] if it does not exist.
func (s *Store) ReleaseClaim(_ context.Context, id string, releasedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	claim, ok := s.usageClaims[id]
	if !ok {
		return store.ErrNotFound
	}

	claim.ReleasedAt = &releasedAt
	s.usageClaims[id] = claim
	return nil
}

// ActiveClaimCount returns the number of claims for the given pool
// where ReleasedAt IS NULL.
func (s *Store) ActiveClaimCount(_ context.Context, poolID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, claim := range s.usageClaims {
		if claim.PoolID == poolID && claim.ReleasedAt == nil {
			count++
		}
	}
	return count, nil
}

// TryClaimSlots atomically checks pool capacity and creates claim
// records for each claim. The fake implementation holds the mutex for the
// duration of the check-and-insert, mirroring the transactional semantics of
// the SQLite implementation.
func (s *Store) TryClaimSlots(
	_ context.Context,
	taskAttemptID string,
	claims []store.UsagePoolClaim,
	claimedAt time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check capacity for every pool before inserting anything.
	for _, c := range claims {
		if c.MaxConcurrent <= 0 {
			continue // unlimited
		}
		active := 0
		for _, co := range s.usageClaims {
			if co.PoolID == c.PoolID && co.ReleasedAt == nil {
				active++
			}
		}
		if active >= c.MaxConcurrent {
			return store.ErrUsageAtCapacity
		}
	}

	// All pools have capacity — insert the claim records.
	for _, c := range claims {
		co := store.UsageClaim{
			ID:            c.ClaimID,
			PoolID:        c.PoolID,
			TaskAttemptID: taskAttemptID,
			ClaimedAt:     claimedAt,
			ReleasedAt:    nil,
		}
		s.usageClaims[co.ID] = co
	}
	return nil
}

// ReleaseAttemptClaims sets ReleasedAt on every active claim for the
// given taskAttemptID and returns the number of claims released.
func (s *Store) ReleaseAttemptClaims(_ context.Context, taskAttemptID string, releasedAt time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for id, co := range s.usageClaims {
		if co.TaskAttemptID == taskAttemptID && co.ReleasedAt == nil {
			co.ReleasedAt = &releasedAt
			s.usageClaims[id] = co
			n++
		}
	}
	return n, nil
}

// ReleaseJobClaims sets ReleasedAt on every active claim held by any
// attempt for tasks belonging to the given job. Returns the number released.
func (s *Store) ReleaseJobClaims(_ context.Context, jobID string, releasedAt time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build the set of task IDs for the job.
	jobTasks := make(map[string]struct{})
	for _, t := range s.tasks {
		if t.JobID == jobID {
			jobTasks[t.ID] = struct{}{}
		}
	}

	// Build the set of attempt IDs for those tasks.
	jobAttempts := make(map[string]struct{})
	for _, a := range s.taskAttempts {
		if _, ok := jobTasks[a.TaskID]; ok {
			jobAttempts[a.ID] = struct{}{}
		}
	}

	n := 0
	for id, co := range s.usageClaims {
		if co.ReleasedAt != nil {
			continue
		}
		if _, ok := jobAttempts[co.TaskAttemptID]; !ok {
			continue
		}
		co.ReleasedAt = &releasedAt
		s.usageClaims[id] = co
		n++
	}
	return n, nil
}
