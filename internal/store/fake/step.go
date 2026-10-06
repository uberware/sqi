// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

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
