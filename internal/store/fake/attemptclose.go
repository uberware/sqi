// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"errors"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// releaseAttemptClaimsLocked releases every active claim held by attemptID and
// returns how many it released. Caller holds s.mu.
func (s *Store) releaseAttemptClaimsLocked(attemptID string, at time.Time) int {
	n := 0
	for id, c := range s.usageClaims {
		if c.TaskAttemptID == attemptID && c.ReleasedAt == nil {
			released := at
			c.ReleasedAt = &released
			s.usageClaims[id] = c
			n++
		}
	}
	return n
}

// closeRunningAttemptLocked closes attemptID if it is still running and
// releases its claims (invariant I3). A redelivery finds the attempt already
// closed and leaves it alone, exactly as SQLite's `WHERE status = 'running'`
// does; the claim release is unconditional in both. Caller holds s.mu.
func (s *Store) closeRunningAttemptLocked(c store.AttemptCompletion) {
	if a, ok := s.taskAttempts[c.AttemptID]; ok && a.Status == store.AttemptStatusRunning {
		ended := c.EndedAt
		a.Status, a.EndedAt, a.ExitCode = c.AttemptStatus, &ended, nil
		if c.ExitCode != nil {
			code := *c.ExitCode
			a.ExitCode = &code
		}
		if c.SessionID != "" {
			a.SessionID = c.SessionID
		}
		if c.Message != "" {
			a.Message = c.Message
		}
		s.taskAttempts[c.AttemptID] = a
	}
	s.releaseAttemptClaimsLocked(c.AttemptID, c.EndedAt)
}

// CompleteTaskAttempt implements [store.TaskStore].
//
// The outcomes match the SQLite store's, which is the reference:
//   - unknown task: [store.ErrNotFound], nothing written;
//   - the task already holds c.TaskStatus: Applied, the task is not rewritten
//     (a redelivery);
//   - the state machine refuses the move: Rejected, but the attempt close and
//     the claim release still happen;
//   - otherwise the task moves and Applied is reported.
//
// In every non-error case the failure reason is stamped only when the task
// ends up holding c.TaskStatus.
func (s *Store) CompleteTaskAttempt(_ context.Context, c store.AttemptCompletion) (store.CompletionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[c.TaskID]
	if !ok {
		return store.CompletionResult{}, store.ErrNotFound
	}

	// Decide the task move before writing anything, so an error return leaves
	// no half-applied state, as SQLite's rolled-back transaction does.
	moves := t.Status != c.TaskStatus
	if moves {
		if err := store.ValidateTaskTransition(t.Status, c.TaskStatus); err != nil {
			if !errors.Is(err, store.ErrInvalidTransition) {
				return store.CompletionResult{}, err
			}
			// Refused: the attempt close and claim release still commit.
			s.closeRunningAttemptLocked(c)
			return store.CompletionResult{Rejected: true}, nil
		}
	}

	s.closeRunningAttemptLocked(c)
	if moves {
		t.Status, t.UnschedulableReason, t.UpdatedAt = c.TaskStatus, "", c.EndedAt
	}
	if c.FailureReason != "" {
		t.FailureReason, t.UpdatedAt = c.FailureReason, c.EndedAt
	}
	s.tasks[c.TaskID] = t
	return store.CompletionResult{Applied: true}, nil
}
