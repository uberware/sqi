// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

// LeaseTask implements [store.TaskStore]. Every outcome is decided under the
// store lock before anything is written, so a non-leased outcome or an error
// changes nothing, as SQLite's rolled-back transaction does.
//
// The outcomes match the SQLite store's, which is the reference:
//   - an unknown task, or one whose job, queue or farm row is missing, is Lost
//     (SQLite's lease joins all three);
//   - a task failing ListReadyTasks' eligibility predicate (isReadyInFarm) is
//     Lost;
//   - the queue cap is checked before the farm cap;
//   - pools are checked in pool-ID order, whatever order the request lists
//     them in, so the same full pool is reported;
//   - a request naming one pool twice sees its own first claim, so it is
//     PoolFull when that fills the pool, and ErrConflict otherwise (SQLite's
//     unique (task_attempt_id, pool_id) index).
func (s *Store) LeaseTask(_ context.Context, req store.LeaseRequest) (store.LeaseResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[req.TaskID]
	if !ok || !s.leasableLocked(t, req) {
		return store.LeaseResult{Outcome: store.LeaseLost}, nil
	}
	if full := s.leaseCapsFullLocked(s.jobs[t.JobID]); full != "" {
		return store.LeaseResult{Outcome: full}, nil
	}
	claims := slices.Clone(req.Claims)
	slices.SortStableFunc(claims, func(a, b store.UsagePoolClaim) int { return cmp.Compare(a.PoolID, b.PoolID) })
	if res, err := s.leasePoolsFullLocked(claims); err != nil || res.Outcome != "" {
		return res, err
	}

	// SQLite stores and returns these times in UTC.
	now := req.Now.UTC()
	at := now
	t.Status, t.AssignedWorkerID, t.AssignedAt, t.UnschedulableReason, t.UpdatedAt = store.TaskStatusAssigned, req.WorkerID, &at, "", now
	s.tasks[t.ID] = t
	num := 1
	if latest, ok := s.latestAttemptLocked(t.ID); ok {
		num = latest.AttemptNumber + 1
	}
	attempt := store.TaskAttempt{
		ID: req.AttemptID, TaskID: t.ID, WorkerID: req.WorkerID, AttemptNumber: num,
		Status: store.AttemptStatusRunning, StartedAt: now, CreatedAt: now,
	}
	s.taskAttempts[attempt.ID] = attempt
	for _, c := range claims {
		s.usageClaims[c.ClaimID] = store.UsageClaim{ID: c.ClaimID, PoolID: c.PoolID, TaskAttemptID: attempt.ID, ClaimedAt: now}
	}
	return store.LeaseResult{Outcome: store.LeaseLeased, Attempt: attempt}, nil
}

// leasableLocked is SQLite's lease guard: the task's job, queue and farm rows
// all exist, and the task passes the eligibility predicate ListReadyTasks
// lists by. Caller holds s.mu.
func (s *Store) leasableLocked(t store.Task, req store.LeaseRequest) bool {
	j, ok := s.jobs[t.JobID]
	if !ok {
		return false
	}
	q, ok := s.queues[j.QueueID]
	if !ok {
		return false
	}
	if _, ok := s.farms[j.FarmID]; !ok {
		return false
	}
	return isReadyInFarm(t, "", req.Now, s.jobs, map[string]bool{q.ID: q.Paused})
}

// leaseCapsFullLocked checks the job's queue cap, then its farm cap, against
// the active tasks in each (counted before the lease, so a count at the cap
// means full). It returns the first full outcome, or "" when both have room.
// Caller holds s.mu.
func (s *Store) leaseCapsFullLocked(j store.Job) store.LeaseOutcome {
	if q := s.queues[j.QueueID]; q.MaxConcurrentTasks > 0 &&
		s.activeTasksLocked(func(x store.Job) bool { return x.QueueID == j.QueueID }) >= q.MaxConcurrentTasks {
		return store.LeaseQueueFull
	}
	if f := s.farms[j.FarmID]; f.MaxConcurrentTasks > 0 &&
		s.activeTasksLocked(func(x store.Job) bool { return x.FarmID == j.FarmID }) >= f.MaxConcurrentTasks {
		return store.LeaseFarmFull
	}
	return ""
}

// leasePoolsFullLocked checks each claim's pool in turn, as SQLite does,
// counting the claims this request has already accepted for the pool. It
// returns a PoolFull result naming the first pool without room (the caller's
// name when the pool is gone), an error for a pool named twice that still has
// room, or a zero result when every claim fits. Caller holds s.mu.
func (s *Store) leasePoolsFullLocked(claims []store.UsagePoolClaim) (store.LeaseResult, error) {
	accepted := make(map[string]int, len(claims))
	for _, c := range claims {
		p, ok := s.usagePools[c.PoolID]
		if !ok {
			return store.LeaseResult{Outcome: store.LeasePoolFull, FullPool: c.PoolName}, nil
		}
		if p.MaxConcurrent > 0 && s.activeClaimsLocked(c.PoolID)+accepted[c.PoolID] >= p.MaxConcurrent {
			return store.LeaseResult{Outcome: store.LeasePoolFull, FullPool: p.Name}, nil
		}
		if accepted[c.PoolID] > 0 {
			return store.LeaseResult{}, fmt.Errorf("fake: insert claim for pool %q: %w", p.Name, store.ErrConflict)
		}
		accepted[c.PoolID]++
	}
	return store.LeaseResult{}, nil
}
