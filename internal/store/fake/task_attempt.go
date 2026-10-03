// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"time"

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

// TerminateWorkerAttempts marks all running attempts for tasks currently
// assigned to workerID as the given terminal status with the supplied end time,
// recording [store.FailureReasonWorkerOffline] as the message. It releases no
// claims and leaves the tasks alone; a worker is taken offline through
// [Store.OfflineStaleWorker] and [Store.OfflineWorker], which do all three.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) TerminateWorkerAttempts(_ context.Context, workerID string, status store.AttemptStatus, endedAt time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Collect task IDs currently assigned to this worker.
	assigned := make(map[string]struct{})
	for _, t := range s.tasks {
		if t.AssignedWorkerID == workerID &&
			(t.Status == store.TaskStatusAssigned || t.Status == store.TaskStatusRunning) {
			assigned[t.ID] = struct{}{}
		}
	}

	var n int
	for id, a := range s.taskAttempts {
		if a.Status != store.AttemptStatusRunning {
			continue
		}
		if _, ok := assigned[a.TaskID]; !ok {
			continue
		}
		a.Status = status
		a.EndedAt = &endedAt
		a.Message = store.FailureReasonWorkerOffline
		s.taskAttempts[id] = a
		n++
	}
	return n, nil
}

// CancelJobAttempts marks all running [store.TaskAttempt] records for tasks
// belonging to the given job as [store.AttemptStatusCanceled]. It releases no
// claims and leaves the tasks alone; a job is canceled through
// [Store.CancelJobExecution], which does all three.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) CancelJobAttempts(_ context.Context, jobID string, endedAt time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build the set of task IDs belonging to this job.
	jobTasks := make(map[string]struct{})
	for _, t := range s.tasks {
		if t.JobID == jobID {
			jobTasks[t.ID] = struct{}{}
		}
	}

	var n int
	for id, a := range s.taskAttempts {
		if a.Status != store.AttemptStatusRunning {
			continue
		}
		if _, ok := jobTasks[a.TaskID]; !ok {
			continue
		}
		a.Status = store.AttemptStatusCanceled
		a.EndedAt = &endedAt
		s.taskAttempts[id] = a
		n++
	}
	return n, nil
}

// UpdateTaskAttempt replaces the mutable fields of an existing attempt
// (Status, ExitCode, EndedAt, and SessionID/Message if non-empty). It writes
// only while the attempt is running: a closed attempt is [store.ErrConflict]
// and is left untouched, as in SQLite (F16).
func (s *Store) UpdateTaskAttempt(_ context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.taskAttempts[attempt.ID]
	if !ok {
		return store.TaskAttempt{}, store.ErrNotFound
	}
	if existing.Status != store.AttemptStatusRunning {
		return store.TaskAttempt{}, store.ErrConflict
	}

	existing.Status = attempt.Status
	existing.ExitCode = attempt.ExitCode
	existing.EndedAt = attempt.EndedAt
	// Only overwrite SessionID when the caller provides a non-empty value,
	// matching the COALESCE(NULLIF(?, ''), session_id) behavior in SQLite.
	if attempt.SessionID != "" {
		existing.SessionID = attempt.SessionID
	}
	// Likewise for Message, matching COALESCE(NULLIF(?, ''), message) in SQLite.
	if attempt.Message != "" {
		existing.Message = attempt.Message
	}
	s.taskAttempts[attempt.ID] = existing
	return existing, nil
}
