// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"context"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

// CreateTaskAttempt inserts a new attempt record.
func (s *Store) CreateTaskAttempt(_ context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.taskAttempts[attempt.ID] = attempt
	return attempt, nil
}

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

// UpdateTaskAttempt replaces the mutable fields of an existing attempt
// (Status, ExitCode, EndedAt).
func (s *Store) UpdateTaskAttempt(_ context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.taskAttempts[attempt.ID]
	if !ok {
		return store.TaskAttempt{}, store.ErrNotFound
	}

	existing.Status = attempt.Status
	existing.ExitCode = attempt.ExitCode
	existing.EndedAt = attempt.EndedAt
	s.taskAttempts[attempt.ID] = existing
	return existing, nil
}
