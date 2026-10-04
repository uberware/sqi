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
		ended := c.EndedAt.UTC() // SQLite stores timeToText(c.EndedAt.UTC())
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
//   - c.AttemptID is not the task's latest attempt (a newer lease superseded
//     it): Rejected, the task and its failure reason are left alone, but the
//     attempt close and the claim release still happen;
//   - the task already holds c.TaskStatus: Applied, the task is not rewritten
//     (a redelivery);
//   - the task is out of flight (not assigned/running) and not already at
//     c.TaskStatus: Rejected, close and release still happen;
//   - the state machine refuses the move: Rejected, but the attempt close and
//     the claim release still happen;
//   - otherwise the task moves and Applied is reported.
//
// In every non-error case the failure reason is stamped only when the task
// ends up holding c.TaskStatus by this attempt's report.
func (s *Store) CompleteTaskAttempt(_ context.Context, c store.AttemptCompletion) (store.CompletionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[c.TaskID]
	if !ok {
		return store.CompletionResult{}, store.ErrNotFound
	}

	// Decide the task move before writing anything, so an error return leaves
	// no half-applied state, as SQLite's rolled-back transaction does. The
	// latest-attempt check comes first, as SQLite runs it before it reads the
	// task's status; it reads only attempt numbers, which the close below does
	// not change, so deciding it before the close is the same as after.
	if !s.isLatestAttemptLocked(c.TaskID, c.AttemptID) {
		// Superseded: the attempt close and claim release still commit.
		s.closeRunningAttemptLocked(c)
		return store.CompletionResult{Rejected: true}, nil
	}
	moves := t.Status != c.TaskStatus
	if moves {
		if t.Status != store.TaskStatusAssigned && t.Status != store.TaskStatusRunning {
			// Out of flight (H4a2 §4.2): refused, but the close and release commit.
			s.closeRunningAttemptLocked(c)
			return store.CompletionResult{Rejected: true}, nil
		}
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
// are released, and finally every open step of the job is finalized. The tasks
// go first, as the interface documents.
func (s *Store) CancelJobExecution(_ context.Context, jobID, reason string, now time.Time) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Order mirrors SQLite's (spec 4.1): the tasks, then the attempts, then the
	// claims, then the steps. The store lock stands in for the job-row anchor.
	now = now.UTC() // SQLite stores and returns these times in UTC
	var active []store.Task
	for id, t := range s.tasks {
		if t.JobID != jobID || terminalTask(t.Status) {
			continue
		}
		if t.Status == store.TaskStatusAssigned || t.Status == store.TaskStatusRunning {
			row := t
			row.Parameters = copyMap(t.Parameters) // a copy, as GetTask returns
			active = append(active, row)
		}
		s.cancelTaskRowLocked(id, reason, now, true)
	}
	s.closeAttemptsAndReleaseClaimsLocked(func(taskID string) bool { return s.tasks[taskID].JobID == jobID }, store.AttemptStatusCanceled, "", now)
	s.cancelJobFinalizeStepsLocked(jobID, now)
	return active, nil
}

// cancelJobFinalizeStepsLocked is SQLite's sqlCancelJobFinalizeSteps: every
// open step of the job becomes terminal. A pending step, or one with no tasks,
// is canceled; any other open step gets FinalizeStep's outcome. A step with a
// task still in flight is left alone. Caller holds s.mu.
func (s *Store) cancelJobFinalizeStepsLocked(jobID string, now time.Time) {
	hasTask := map[string]bool{}
	for _, t := range s.tasks {
		hasTask[t.StepID] = true
	}
	for id, st := range s.steps {
		if st.JobID != jobID || terminalStep(st.Status) {
			continue
		}
		out := s.stepOutcomeLocked(id)
		switch {
		case out == "":
			continue // a task is still in flight
		case st.Status == store.StepStatusPending || !hasTask[id]:
			out = store.StepStatusCanceled
		}
		st.Status, st.UpdatedAt = out, now
		s.steps[id] = st
	}
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
	prior.Parameters = copyMap(prior.Parameters) // a copy, as GetTask returns
	if terminalTask(prior.Status) {
		return prior, false, nil
	}
	// Same order as the job cancel: the task, then its attempt, then the claims.
	now = now.UTC()
	s.cancelTaskRowLocked(taskID, reason, now, false)
	s.closeAttemptsAndReleaseClaimsLocked(func(id string) bool { return id == taskID }, store.AttemptStatusCanceled, "", now)
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

// closeAttemptsAndReleaseClaimsLocked is invariant I3 for a cancel, a reap or an
// offline reclaim: it closes every running attempt whose task matches with
// status, ended at now, and then releases the active claims of every matching
// attempt that is no longer running. The release is by attempt status, as
// SQLite's is, so a claim is never released while its attempt is open. A
// non-empty message is recorded over the attempt's own and an empty one leaves
// it, as SQLite's close does (it keeps the stored message when given none).
// Caller holds s.mu.
func (s *Store) closeAttemptsAndReleaseClaimsLocked(matches func(taskID string) bool, status store.AttemptStatus, message string, now time.Time) {
	for id, a := range s.taskAttempts {
		if !matches(a.TaskID) || a.Status != store.AttemptStatusRunning {
			continue
		}
		ended := now
		a.Status, a.EndedAt = status, &ended
		if message != "" {
			a.Message = message
		}
		s.taskAttempts[id] = a
	}
	for id, a := range s.taskAttempts {
		if matches(a.TaskID) && a.Status != store.AttemptStatusRunning {
			s.releaseAttemptClaimsLocked(id, now)
		}
	}
}

// OfflineStaleWorker implements [store.WorkerStore].
//
// The outcomes match the SQLite store's, which is the reference: the guard
// admits a worker that is online, or disabled with work in flight (H4a2 §5.2),
// and whose heartbeat is strictly older than cutoff (a worker with no recorded
// heartbeat is never stale, as SQL's NULL comparison has it, and an unknown
// worker is simply not stale); otherwise nothing is written. See
// [Store.offlineWorkerLocked] for what a match does.
func (s *Store) OfflineStaleWorker(_ context.Context, id string, cutoff, now time.Time) ([]store.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.workers[id]
	if !ok || w.LastHeartbeatAt == nil || !w.LastHeartbeatAt.Before(cutoff) ||
		(w.Status != store.WorkerStatusOnline && (w.Status != store.WorkerStatusDisabled || !s.hasWorkInFlightLocked(id))) {
		return nil, false, nil
	}
	return s.offlineWorkerLocked(id, now), true, nil
}

// OfflineWorker implements [store.WorkerStore]. An unknown worker is
// [store.ErrNotFound], as SQLite's zero-row mark is; any other known worker
// goes offline, except a disabled one, which stays disabled (H4a2 §5.3).
func (s *Store) OfflineWorker(_ context.Context, id string, now time.Time) ([]store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[id]; !ok {
		return nil, store.ErrNotFound
	}
	return s.offlineWorkerLocked(id, now), nil
}

// offlineWorkerLocked takes the worker offline (a disabled worker keeps its
// status, H4a2 §5.2 and §5.3), returns its assigned and running
// tasks to ready, then closes those tasks' running attempts as failed with
// [store.FailureReasonWorkerOffline] and releases the claims of their closed
// attempts (invariant I3). It returns the reclaimed tasks as they are after the
// reset, as SQLite's RETURNING does. The order mirrors SQLite's (spec 4.1): the
// worker, the tasks, then the attempts and claims; the store lock stands in for
// the worker-row and job-row anchors. Caller holds s.mu and has checked the
// worker exists.
func (s *Store) offlineWorkerLocked(id string, now time.Time) []store.Task {
	now = now.UTC() // SQLite stores and returns these times in UTC
	w := s.workers[id]
	if w.Status != store.WorkerStatusDisabled {
		w.Status = store.WorkerStatusOffline
	}
	w.UpdatedAt = now
	s.workers[id] = w

	return s.reclaimToReadyLocked(func(t store.Task) bool {
		return t.AssignedWorkerID == id && (t.Status == store.TaskStatusAssigned || t.Status == store.TaskStatusRunning)
	}, store.FailureReasonWorkerOffline, now)
}

// reclaimToReadyLocked is the shared reclaim block of the reaper and the offline
// transitions, the fake's counterpart of SQLite's one reset statement
// (sqlReclaimStaleAssignedTasks / sqlReclaimWorkerTasks, both RETURNING). It
// returns every task match accepts to [store.TaskStatusReady] with its worker
// and assignment time cleared and its unschedulable reason emptied, then closes
// those tasks' running attempts as failed with message and releases the claims of
// their closed attempts (invariant I3). It returns the reclaimed tasks as they
// are after the reset, with Parameters copied, as RETURNING does, or nil when
// nothing matched. The tasks come first, then the attempts, then the claims
// (spec 4.1). now is stamped as given; callers pass UTC. Caller holds s.mu.
func (s *Store) reclaimToReadyLocked(match func(store.Task) bool, message string, now time.Time) []store.Task {
	reclaimed := make(map[string]bool)
	var out []store.Task
	for id, t := range s.tasks {
		if !match(t) {
			continue
		}
		t.Status, t.AssignedWorkerID, t.AssignedAt, t.UnschedulableReason, t.UpdatedAt = store.TaskStatusReady, "", nil, "", now
		s.tasks[id] = t
		reclaimed[id] = true
		row := t
		row.Parameters = copyMap(t.Parameters)
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil
	}
	s.closeAttemptsAndReleaseClaimsLocked(func(taskID string) bool { return reclaimed[taskID] }, store.AttemptStatusFailed, message, now)
	return out
}
