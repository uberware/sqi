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
	// released_at is server time, like the task row's updated_at: c.EndedAt is
	// the worker's clock and feeds only the attempt's ended_at above.
	s.releaseAttemptClaimsLocked(c.AttemptID, time.Now().UTC())
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
	// The task row's updated_at is server time, as UpdateTaskStatus stamps it:
	// c.EndedAt comes from the worker's clock and belongs to the attempt only.
	now := time.Now().UTC()
	if moves {
		t.Status, t.UnschedulableReason, t.UpdatedAt = c.TaskStatus, "", now
	}
	if c.FailureReason != "" {
		t.FailureReason, t.UpdatedAt = c.FailureReason, now
	}
	s.tasks[c.TaskID] = t
	return store.CompletionResult{Applied: true}, nil
}

// CancelJobExecution implements [store.TaskStore].
//
// The outcomes match the SQLite store's, which is the reference: every
// non-terminal task of the job is canceled with its worker assignment cleared,
// then every running attempt of the job's tasks (including one on a task that
// was already terminal) is closed, then the claims of the job's closed attempts
// are released. The tasks go first, as the interface documents.
func (s *Store) CancelJobExecution(_ context.Context, jobID, reason string, now time.Time) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Order mirrors SQLite's (spec 4.1): the tasks, then the attempts, then the
	// claims. The store lock stands in for the job-row anchor.
	now = now.UTC() // SQLite stores and returns these times in UTC
	var active []store.Task
	for id, t := range s.tasks {
		if t.JobID != jobID || terminalTask(t.Status) {
			continue
		}
		if t.Status == store.TaskStatusAssigned || t.Status == store.TaskStatusRunning {
			active = append(active, t)
		}
		s.cancelTaskRowLocked(id, reason, now, true)
	}
	s.closeAttemptsAndReleaseClaimsLocked(func(taskID string) bool { return s.tasks[taskID].JobID == jobID }, now)
	return active, nil
}

// CancelTaskExecution implements [store.TaskStore].
//
// The outcomes match the SQLite store's: an unknown task is
// [store.ErrNotFound]; a terminal task is returned unchanged with false and
// nothing is written (its attempts and claims are left to whatever closed it);
// otherwise the task is canceled with its worker assignment kept, then its
// running attempt is closed and its closed attempts' claims released.
func (s *Store) CancelTaskExecution(_ context.Context, taskID, reason string, now time.Time) (store.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prior, ok := s.tasks[taskID]
	if !ok {
		return store.Task{}, false, store.ErrNotFound
	}
	if terminalTask(prior.Status) {
		return prior, false, nil
	}
	// Same order as the job cancel: the task, then its attempt, then the claims.
	now = now.UTC()
	s.cancelTaskRowLocked(taskID, reason, now, false)
	s.closeAttemptsAndReleaseClaimsLocked(func(id string) bool { return id == taskID }, now)
	return prior, true, nil
}

// cancelTaskRowLocked moves one non-terminal task to canceled and stamps reason
// when the task has none. A job-wide cancel clears the worker assignment
// (clearAssignment); a single-task cancel leaves it, as SQLite's two statements
// do. Caller holds s.mu.
func (s *Store) cancelTaskRowLocked(taskID, reason string, now time.Time, clearAssignment bool) {
	t := s.tasks[taskID]
	t.Status, t.UnschedulableReason, t.UpdatedAt = store.TaskStatusCanceled, "", now
	if clearAssignment {
		t.AssignedWorkerID, t.AssignedAt = "", nil
	}
	if reason != "" && t.FailureReason == "" {
		t.FailureReason = reason
	}
	s.tasks[taskID] = t
}

// closeAttemptsAndReleaseClaimsLocked is invariant I3 for a cancel: it closes
// every running attempt whose task matches, as canceled and ended at now, and
// then releases the active claims of every matching attempt that is no longer
// running. The release is by attempt status, as SQLite's is, so a claim is never
// released while its attempt is open. Caller holds s.mu.
func (s *Store) closeAttemptsAndReleaseClaimsLocked(matches func(taskID string) bool, now time.Time) {
	for id, a := range s.taskAttempts {
		if !matches(a.TaskID) || a.Status != store.AttemptStatusRunning {
			continue
		}
		ended := now
		a.Status, a.EndedAt = store.AttemptStatusCanceled, &ended
		s.taskAttempts[id] = a
	}
	for id, a := range s.taskAttempts {
		if matches(a.TaskID) && a.Status != store.AttemptStatusRunning {
			s.releaseAttemptClaimsLocked(id, now)
		}
	}
}
