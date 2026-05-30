// SPDX-License-Identifier: AGPL-3.0-only

package fake

import (
	"context"
	"slices"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateStep inserts a new step. The (JobID, Name) pair must be unique
// within the job; returns [store.ErrConflict] if violated.
func (s *Store) CreateStep(_ context.Context, step store.Step) (store.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.steps {
		if existing.JobID == step.JobID && existing.Name == step.Name {
			return store.Step{}, store.ErrConflict
		}
	}

	step.DependsOn = copySlice(step.DependsOn)
	s.steps[step.ID] = step
	return step, nil
}

// GetStep returns the step with the given ID, or [store.ErrNotFound].
func (s *Store) GetStep(_ context.Context, id string) (store.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	step, ok := s.steps[id]
	if !ok {
		return store.Step{}, store.ErrNotFound
	}

	step.DependsOn = copySlice(step.DependsOn)
	return step, nil
}

// ListSteps returns all steps for the given job, ordered by StepOrder ascending.
func (s *Store) ListSteps(_ context.Context, jobID string) ([]store.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	steps := make([]store.Step, 0)
	for _, step := range s.steps {
		if step.JobID == jobID {
			step.DependsOn = copySlice(step.DependsOn)
			steps = append(steps, step)
		}
	}

	slices.SortStableFunc(steps, func(a, b store.Step) int {
		if a.StepOrder < b.StepOrder {
			return -1
		}
		if a.StepOrder > b.StepOrder {
			return 1
		}
		return 0
	})

	return steps, nil
}

// UpdateStepStatus transitions a step to a new status and updates UpdatedAt.
func (s *Store) UpdateStepStatus(_ context.Context, id string, status store.StepStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	step, ok := s.steps[id]
	if !ok {
		return store.ErrNotFound
	}

	step.Status = status
	step.UpdatedAt = time.Now()
	s.steps[id] = step
	return nil
}
