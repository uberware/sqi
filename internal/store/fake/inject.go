// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"

	"github.com/uberware/sqi/internal/store"
)

// InjectTaskAttempt stores attempt exactly as given, with no state checks.
//
// Corruption injection for invariant, recovery and repair tests: it exists to
// build states production cannot reach, and must never be used to seed a
// reachable one — seed through CreateJobSubmission and production writes
// instead (internal/store/storetest). It is not part of store.Store. Unlike
// SQLite, the fake has no foreign keys, so an orphan attempt is accepted.
func (s *Store) InjectTaskAttempt(_ context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.taskAttempts[attempt.ID] = attempt
	return attempt, nil
}

// InjectClaim stores an active claim exactly as given. Same contract as
// [Store.InjectTaskAttempt]. The (TaskAttemptID, PoolID) pair must be unique
// among active claims, as SQLite's index requires; it returns
// [store.ErrConflict] if it is not.
func (s *Store) InjectClaim(_ context.Context, claim store.UsageClaim) (store.UsageClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.usageClaims {
		if existing.TaskAttemptID == claim.TaskAttemptID && existing.PoolID == claim.PoolID && existing.ReleasedAt == nil {
			return store.UsageClaim{}, store.ErrConflict
		}
	}
	s.usageClaims[claim.ID] = claim
	return claim, nil
}
