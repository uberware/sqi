// SPDX-License-Identifier: AGPL-3.0-only

package openjd

import (
	"context"
	"fmt"

	"github.com/uberware/sqi/internal/store"
)

// ResolveDependencies inspects every step of jobID and promotes any
// [store.StepStatusPending] step whose declared dependencies have all reached
// [store.StepStatusCompleted] to [store.StepStatusReady], then transitions
// all of that step's [store.TaskStatusPending] tasks to [store.TaskStatusReady].
//
// It returns the number of steps newly promoted to ready.
//
// Calling ResolveDependencies is idempotent: steps that are already past
// pending are skipped. It is typically invoked by the scheduler immediately
// after any step transitions to [store.StepStatusCompleted].
//
// Steps with an empty DependsOn list are always eligible. In normal operation
// such steps are created with [store.StepStatusReady] by [Submitter.Submit], so
// they will simply be skipped here. If a no-dep step is somehow in pending
// state, ResolveDependencies will promote it correctly.
//
// Note: ResolveDependencies considers only a single pass through the step list.
// Call it again if the first pass unblocked a step whose promotion might
// unblock further steps in the same job.
func ResolveDependencies(ctx context.Context, st store.Store, jobID string) (int, error) {
	steps, err := st.ListSteps(ctx, jobID)
	if err != nil {
		return 0, fmt.Errorf("openjd: resolve deps for job %s: list steps: %w", jobID, err)
	}

	// Build name→status lookup used for dependency checking.
	statusByName := make(map[string]store.StepStatus, len(steps))
	for _, s := range steps {
		statusByName[s.Name] = s.Status
	}

	var promoted int
	for _, step := range steps {
		if step.Status != store.StepStatusPending {
			continue
		}

		if !allDepsCompleted(step.DependsOn, statusByName) {
			continue
		}

		// All deps are satisfied — promote the step.
		if err := st.UpdateStepStatus(ctx, step.ID, store.StepStatusReady); err != nil {
			return promoted, fmt.Errorf(
				"openjd: resolve deps for job %s: update step %s to ready: %w",
				jobID, step.ID, err,
			)
		}

		// Promote every pending task in this step.
		if err := markStepTasksReady(ctx, st, jobID, step.ID); err != nil {
			return promoted, err
		}

		promoted++
	}

	return promoted, nil
}

// allDepsCompleted reports whether every name in deps maps to
// [store.StepStatusCompleted] in statusByName.
// An empty deps slice returns true — no dependencies means always eligible.
func allDepsCompleted(deps []string, statusByName map[string]store.StepStatus) bool {
	for _, name := range deps {
		if statusByName[name] != store.StepStatusCompleted {
			return false
		}
	}
	return true
}

// markStepTasksReady transitions every pending task in stepID to
// [store.TaskStatusReady].
//
// Tasks are fetched with [store.MaxLimit] per page. Steps with more than
// [store.MaxLimit] tasks require multiple passes; for Phase 1 workloads
// this limit is not expected to be reached.
func markStepTasksReady(ctx context.Context, st store.Store, jobID, stepID string) error {
	opts := store.ListTasksOptions{
		StepID: stepID,
		Status: store.TaskStatusPending,
		Pagination: store.Pagination{
			Limit: store.MaxLimit,
		},
	}

	page, err := st.ListTasks(ctx, opts)
	if err != nil {
		return fmt.Errorf(
			"openjd: resolve deps for job %s: list pending tasks for step %s: %w",
			jobID, stepID, err,
		)
	}

	for _, task := range page.Items {
		if err := st.UpdateTaskStatus(ctx, task.ID, store.TaskStatusReady); err != nil {
			return fmt.Errorf(
				"openjd: resolve deps for job %s: update task %s to ready: %w",
				jobID, task.ID, err,
			)
		}
	}

	return nil
}
