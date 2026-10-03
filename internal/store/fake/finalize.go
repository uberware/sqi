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
	st.Status, st.UpdatedAt = out, now
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
	at := now
	j.Status, j.CompletedAt, j.UpdatedAt = out, &at, now
	s.jobs[id] = j
	return out, true, nil
}

// ListStuckSteps implements [store.StepStore].
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
