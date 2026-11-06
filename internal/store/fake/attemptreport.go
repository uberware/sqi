// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// liveAttemptLocked reports whether a report from attemptID may act on t, as
// SQLite's liveAttemptTx does: the attempt is still running and is the task's
// latest, and the task is assigned or running. Caller holds s.mu.
func (s *Store) liveAttemptLocked(attemptID string, t store.Task) bool {
	return inFlightTask(t.Status) && s.isLatestAttemptLocked(t.ID, attemptID) &&
		s.taskAttempts[attemptID].Status == store.AttemptStatusRunning
}

// StartTaskAttempt implements [store.TaskStore]; outcomes match SQLite's.
func (s *Store) StartTaskAttempt(ctx context.Context, attemptID, taskID, sessionID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return false, err
	}
	t, ok := s.tasks[taskID]
	if !ok {
		return false, store.ErrNotFound
	}
	if !s.liveAttemptLocked(attemptID, t) {
		return false, nil
	}
	if t.Status == store.TaskStatusAssigned {
		t.Status, t.UnschedulableReason, t.UpdatedAt = store.TaskStatusRunning, "", now.UTC()
		s.tasks[taskID] = t
	}
	if sessionID != "" {
		a := s.taskAttempts[attemptID]
		a.SessionID = sessionID
		s.taskAttempts[attemptID] = a
	}
	return true, nil
}

// RecordAttemptProgress implements [store.TaskStore]; outcomes match SQLite's.
func (s *Store) RecordAttemptProgress(ctx context.Context, p store.AttemptProgress) (store.ProgressOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.ProgressNotLive, err
	}
	t, ok := s.tasks[p.TaskID]
	if !ok {
		return store.ProgressNotLive, store.ErrNotFound
	}
	if !s.liveAttemptLocked(p.AttemptID, t) {
		return store.ProgressNotLive, nil
	}
	if t.Status == store.TaskStatusAssigned {
		return store.ProgressTaskAssigned, nil
	}
	a := s.taskAttempts[p.AttemptID]
	at := p.At.UTC()
	if a.ProgressAt != nil && !a.ProgressAt.Before(at) {
		return store.ProgressOutOfOrder, nil
	}
	a.Progress, a.StatusText, a.ProgressAt = ptrCopy(p.Progress), p.StatusText, &at
	s.taskAttempts[p.AttemptID] = a
	return store.ProgressRecorded, nil
}

// ReclaimTaskAttempt implements [store.TaskStore]; outcomes match SQLite's.
func (s *Store) ReclaimTaskAttempt(ctx context.Context, attemptID, taskID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return false, err
	}
	t, ok := s.tasks[taskID]
	if !ok {
		return false, store.ErrNotFound
	}
	if !s.liveAttemptLocked(attemptID, t) {
		return false, nil
	}
	s.reclaimToReadyLocked(func(c store.Task) bool { return c.ID == taskID }, store.FailureReasonWorkerShutdown, now.UTC())
	return true, nil
}
