// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func terminalTask(s store.TaskStatus) bool {
	return s == store.TaskStatusSucceeded || s == store.TaskStatusFailed || s == store.TaskStatusCanceled
}

func terminalStep(s store.StepStatus) bool {
	return s == store.StepStatusCompleted || s == store.StepStatusFailed || s == store.StepStatusCanceled
}

// stepOutcomeLocked returns the status FinalizeStep would write, or "" while
// any task is non-terminal. Caller holds s.mu.
func (s *Store) stepOutcomeLocked(stepID string) store.StepStatus {
	failed, canceled := false, false
	for _, t := range s.tasks {
		if t.StepID != stepID {
			continue
		}
		switch {
		case !terminalTask(t.Status):
			return ""
		case t.Status == store.TaskStatusFailed:
			failed = true
		case t.Status == store.TaskStatusCanceled:
			canceled = true
		}
	}
	switch {
	case failed:
		return store.StepStatusFailed
	case canceled:
		return store.StepStatusCanceled
	default:
		return store.StepStatusCompleted
	}
}

// FinalizeStep implements [store.StepStore].
func (s *Store) FinalizeStep(_ context.Context, id string, now time.Time) (store.StepStatus, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.steps[id]
	if !ok {
		return "", false, store.ErrNotFound
	}
	if terminalStep(st.Status) {
		return st.Status, false, nil
	}
	out := s.stepOutcomeLocked(id)
	if out == "" {
		return "", false, nil
	}
	st.Status, st.UpdatedAt = out, now.UTC() // SQLite stores timeToText(now.UTC())
	s.steps[id] = st
	return out, true, nil
}

// FinalizeJob implements [store.JobStore].
func (s *Store) FinalizeJob(_ context.Context, id string, now time.Time) (store.JobStatus, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return "", false, store.ErrNotFound
	}
	if j.Status.IsTerminal() {
		return j.Status, false, nil
	}
	failed, canceled := false, false
	for _, st := range s.steps {
		if st.JobID != id {
			continue
		}
		switch {
		case !terminalStep(st.Status):
			return "", false, nil
		case st.Status == store.StepStatusFailed:
			failed = true
		case st.Status == store.StepStatusCanceled:
			canceled = true
		}
	}
	out := store.JobStatusCompleted
	if failed {
		out = store.JobStatusFailed
	} else if canceled {
		out = store.JobStatusCanceled
	}
	at := now.UTC() // SQLite stores timeToText(now.UTC())
	j.Status, j.CompletedAt, j.UpdatedAt = out, &at, at
	s.jobs[id] = j
	return out, true, nil
}

// ListStuckSteps implements [store.StepStore]. Like the SQLite query it lists
// only steps of a job that exists and is not terminal: a terminal job has no
// downstream that needs its steps finalized.
func (s *Store) ListStuckSteps(_ context.Context) ([]store.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hasTask := map[string]bool{}
	for _, t := range s.tasks {
		hasTask[t.StepID] = true
	}
	var out []store.Step
	for id, st := range s.steps {
		if terminalStep(st.Status) || !hasTask[id] || s.stepOutcomeLocked(id) == "" {
			continue
		}
		// SQLite's EXISTS on jobs also excludes a step whose job row is missing.
		if j, ok := s.jobs[st.JobID]; !ok || j.Status.IsTerminal() {
			continue
		}
		st.DependsOn = copySlice(st.DependsOn)
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b store.Step) int {
		if c := strings.Compare(a.JobID, b.JobID); c != 0 {
			return c
		}
		return a.StepOrder - b.StepOrder
	})
	return out, nil
}

// ListJobIDsWithPendingSteps implements [store.StepStore]. Like the SQLite
// query it lists only jobs that exist, are not terminal and are not blocked.
func (s *Store) ListJobIDsWithPendingSteps(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var ids []string
	for _, st := range s.steps {
		if st.Status != store.StepStatusPending || seen[st.JobID] {
			continue
		}
		j, ok := s.jobs[st.JobID]
		if !ok || j.Status.IsTerminal() || j.Status == store.JobStatusBlocked {
			continue
		}
		seen[st.JobID] = true
		ids = append(ids, st.JobID)
	}
	slices.Sort(ids)
	return ids, nil
}

// ReleaseStep implements [store.StepStore].
func (s *Store) ReleaseStep(_ context.Context, id string, now time.Time) (bool, []store.Task, error) {
	return s.movePendingStep(id, store.StepStatusReady, store.TaskStatusReady, "", now)
}

// CancelPendingStep implements [store.StepStore].
func (s *Store) CancelPendingStep(_ context.Context, id, reason string, now time.Time) (bool, []store.Task, error) {
	return s.movePendingStep(id, store.StepStatusCanceled, store.TaskStatusCanceled, reason, now)
}

// movePendingStep moves a pending step and its pending tasks together under
// one lock. Like the SQLite version it writes nothing unless the step is
// still pending, and it stamps reason only on tasks that carry none.
func (s *Store) movePendingStep(id string, stepTo store.StepStatus, taskTo store.TaskStatus, reason string, now time.Time) (bool, []store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.steps[id]
	if !ok {
		return false, nil, store.ErrNotFound
	}
	if st.Status != store.StepStatusPending {
		return false, nil, nil
	}
	now = now.UTC() // SQLite stores UTC; keep the fake's times in the same zone.
	st.Status, st.UpdatedAt = stepTo, now
	s.steps[id] = st
	return true, s.transitionPendingTasksLocked(id, taskTo, reason, now), nil
}

// ReleaseBlockedJob implements [store.JobStore].
func (s *Store) ReleaseBlockedJob(_ context.Context, id string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return false, store.ErrNotFound
	}
	if j.Status != store.JobStatusBlocked {
		return false, nil
	}
	// Like the SQLite statement, an edge whose upstream no longer exists is
	// unsatisfied: the edge survives the upstream's deletion on purpose.
	for _, up := range s.jobDependencies[id] {
		if u, ok := s.jobs[up]; !ok || u.Status != store.JobStatusCompleted {
			return false, nil
		}
	}
	j.Status, j.UpdatedAt = store.JobStatusPending, now.UTC() // SQLite stores UTC.
	s.jobs[id] = j
	return true, nil
}

// CancelBlockedJob implements [store.JobStore].
func (s *Store) CancelBlockedJob(_ context.Context, id, reason string, now time.Time) (bool, []store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return false, nil, store.ErrNotFound
	}
	if j.Status != store.JobStatusBlocked {
		return false, nil, nil
	}
	now = now.UTC() // SQLite stores UTC; keep the fake's times in the same zone.
	at := now
	j.Status, j.CompletedAt, j.UpdatedAt = store.JobStatusCanceled, &at, now
	s.jobs[id] = j
	for sid, st := range s.steps {
		if st.JobID == id && !terminalStep(st.Status) {
			st.Status, st.UpdatedAt = store.StepStatusCanceled, now
			s.steps[sid] = st
		}
	}
	tasks := s.transitionPendingTasksWhereLocked(
		func(t store.Task) bool { return t.JobID == id }, store.TaskStatusCanceled, reason, now,
	)
	return true, tasks, nil
}

// PromoteJobRunning implements [store.JobStore]. Like the SQLite statement it
// writes only a pending job and reports false, without error, for an unknown
// job.
func (s *Store) PromoteJobRunning(_ context.Context, id string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.Status != store.JobStatusPending {
		return false, nil
	}
	now = now.UTC() // SQLite stores UTC; keep the fake's times in the same zone.
	if j.StartedAt == nil {
		at := now
		j.StartedAt = &at
	}
	j.Status, j.UpdatedAt = store.JobStatusRunning, now
	s.jobs[id] = j
	return true, nil
}

// PauseJob implements [store.JobStore].
func (s *Store) PauseJob(_ context.Context, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return store.ErrNotFound
	}
	if j.Status != store.JobStatusPending && j.Status != store.JobStatusRunning {
		return store.ErrConflict
	}
	j.Status, j.UpdatedAt = store.JobStatusPaused, now.UTC() // SQLite stores UTC.
	s.jobs[id] = j
	return nil
}
