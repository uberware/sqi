// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

// GetTaskAttempt returns the attempt with the given ID, or [store.ErrNotFound].
func (s *Store) GetTaskAttempt(_ context.Context, id string) (store.TaskAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	attempt, ok := s.taskAttempts[id]
	if !ok {
		return store.TaskAttempt{}, store.ErrNotFound
	}
	return attempt, nil
}

// LatestTaskAttempt returns the attempt with the highest AttemptNumber for
// the given task, or [store.ErrNotFound] if no attempts exist.
func (s *Store) LatestTaskAttempt(_ context.Context, taskID string) (store.TaskAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, exists := s.latestAttemptLocked(taskID)
	if !exists {
		return store.TaskAttempt{}, store.ErrNotFound
	}
	return found, nil
}

// latestAttemptLocked returns taskID's attempt with the highest AttemptNumber,
// and false when the task has none. Caller holds s.mu.
func (s *Store) latestAttemptLocked(taskID string) (store.TaskAttempt, bool) {
	var found store.TaskAttempt
	var exists bool
	for _, a := range s.taskAttempts {
		if a.TaskID != taskID {
			continue
		}
		if !exists || a.AttemptNumber > found.AttemptNumber {
			found = a
			exists = true
		}
	}
	return found, exists
}

// isLatestAttemptLocked reports whether attemptID is one of taskID's attempts
// with the highest AttemptNumber, as SQLite's sqlIsLatestAttempt does: the
// numbers are compared, not the IDs, so two attempts sharing the highest number
// are both latest there and here. An attempt that does not exist, or that
// belongs to another task, is not. Caller holds s.mu.
func (s *Store) isLatestAttemptLocked(taskID, attemptID string) bool {
	a, ok := s.taskAttempts[attemptID]
	if !ok || a.TaskID != taskID {
		return false
	}
	latest, _ := s.latestAttemptLocked(taskID) // exists: a is one of the task's attempts
	return a.AttemptNumber == latest.AttemptNumber
}

// ListTaskAttempts returns all attempts for the given task, ordered by
// AttemptNumber ascending.
func (s *Store) ListTaskAttempts(_ context.Context, taskID string) ([]store.TaskAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	attempts := make([]store.TaskAttempt, 0)
	for _, attempt := range s.taskAttempts {
		if attempt.TaskID == taskID {
			attempts = append(attempts, attempt)
		}
	}

	slices.SortStableFunc(attempts, func(a, b store.TaskAttempt) int {
		if a.AttemptNumber < b.AttemptNumber {
			return -1
		}
		if a.AttemptNumber > b.AttemptNumber {
			return 1
		}
		return 0
	})

	return attempts, nil
}
