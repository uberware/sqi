// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// InjectTaskAttempt stores attempt as given, with no state checks; a zero
// CreatedAt is stamped now.
//
// Corruption injection for invariant, recovery and repair tests: it exists to
// build states production cannot reach, and must never be used to seed a
// reachable one — seed through CreateJobSubmission and production writes
// instead (internal/store/storetest). It is not part of store.Store. Unlike
// SQLite, the fake has no foreign keys, so an orphan attempt is accepted; a
// reused ID is [store.ErrConflict], as SQLite's primary key makes it.
func (s *Store) InjectTaskAttempt(_ context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.taskAttempts[attempt.ID]; ok {
		return store.TaskAttempt{}, store.ErrConflict
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = time.Now().UTC()
	}
	s.taskAttempts[attempt.ID] = attempt
	return attempt, nil
}

// InjectClaim stores claim active, as SQLite inserts it: a zero ClaimedAt is
// stamped now and ReleasedAt is ignored. Same contract as
// [Store.InjectTaskAttempt]. The ID must be new and the (TaskAttemptID,
// PoolID) pair unique among active claims, as SQLite's primary key and index
// require; it returns [store.ErrConflict] if either is not.
func (s *Store) InjectClaim(_ context.Context, claim store.UsageClaim) (store.UsageClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.usageClaims[claim.ID]; ok {
		return store.UsageClaim{}, store.ErrConflict
	}
	for _, existing := range s.usageClaims {
		if existing.TaskAttemptID == claim.TaskAttemptID && existing.PoolID == claim.PoolID && existing.ReleasedAt == nil {
			return store.UsageClaim{}, store.ErrConflict
		}
	}
	if claim.ClaimedAt.IsZero() {
		claim.ClaimedAt = time.Now().UTC()
	}
	claim.ReleasedAt = nil
	s.usageClaims[claim.ID] = claim
	return claim, nil
}
