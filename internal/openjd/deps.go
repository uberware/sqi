// SPDX-License-Identifier: AGPL-3.0-or-later

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

	statusByName := stepStatusByName(steps)

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
		if _, err := st.TransitionStepPendingTasks(ctx, step.ID, store.TaskStatusReady, ""); err != nil {
			return promoted, fmt.Errorf(
				"openjd: resolve deps for job %s: ready pending tasks for step %s: %w",
				jobID, step.ID, err,
			)
		}

		promoted++
	}

	return promoted, nil
}

// stepStatusByName builds a name→status lookup over steps, used for dependency
// resolution and cancellation decisions.
func stepStatusByName(steps []store.Step) map[string]store.StepStatus {
	m := make(map[string]store.StepStatus, len(steps))
	for _, s := range steps {
		m[s.Name] = s.Status
	}
	return m
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

// CancelDependents cancels every [store.StepStatusPending] step of jobID whose
// dependency graph includes a step that terminated unsuccessfully — that is,
// reached [store.StepStatusFailed] or [store.StepStatusCanceled] — and cancels
// each such step's [store.TaskStatusPending] tasks.
//
// A pending step that depends on a failed or canceled step can never become
// ready (ResolveDependencies only promotes steps whose dependencies all reached
// completed), so it would otherwise strand forever and prevent the job from ever
// reaching a terminal state. Canceling it lets job-completion detection proceed.
//
// It returns the number of steps newly canceled and the pending tasks that were
// transitioned to canceled (so the caller can fan those terminal task transitions
// out to subscribers). Each canceled task is stamped with
// [store.FailureReasonUpstreamFailed] in the same UPDATE, unless it already
// carries a more specific reason.
//
// CancelDependents loops to a fixpoint: canceling a step makes it an
// unsuccessful dependency for its own dependents, so a single call propagates
// through transitive dependency chains. Calling it is idempotent — steps already
// past pending are skipped.
//
// It is typically invoked by the scheduler immediately after any step
// transitions to failed or canceled.
func CancelDependents(ctx context.Context, st store.Store, jobID string) (int, []store.Task, error) {
	steps, err := st.ListSteps(ctx, jobID)
	if err != nil {
		return 0, nil, fmt.Errorf("openjd: cancel dependents for job %s: list steps: %w", jobID, err)
	}

	// Build name→status lookup. Dependency edges are immutable and this function
	// is the only writer of the statuses it inspects, so we list once and keep the
	// map authoritative as we cancel — updating it in place lets a later pass see
	// a step we just canceled, which is how transitive chains resolve.
	statusByName := stepStatusByName(steps)

	var (
		canceled      int
		canceledTasks []store.Task
	)
	// Loop until a full pass cancels nothing more. Each cancellation can unblock
	// further cancellations downstream, so we repeat until the graph is stable.
	for {
		before := canceled
		for _, step := range steps {
			if statusByName[step.Name] != store.StepStatusPending {
				continue
			}

			if !anyDepUnsuccessful(step.DependsOn, statusByName) {
				continue
			}

			// A dependency can never complete — cancel the step and its pending tasks.
			if err := st.UpdateStepStatus(ctx, step.ID, store.StepStatusCanceled); err != nil {
				return canceled, canceledTasks, fmt.Errorf(
					"openjd: cancel dependents for job %s: cancel step %s: %w",
					jobID, step.ID, err,
				)
			}
			tasks, err := st.TransitionStepPendingTasks(ctx, step.ID, store.TaskStatusCanceled, store.FailureReasonUpstreamFailed)
			if err != nil {
				return canceled, canceledTasks, fmt.Errorf(
					"openjd: cancel dependents for job %s: cancel pending tasks for step %s: %w",
					jobID, step.ID, err,
				)
			}
			canceledTasks = append(canceledTasks, tasks...)

			statusByName[step.Name] = store.StepStatusCanceled
			canceled++
		}

		if canceled == before {
			return canceled, canceledTasks, nil
		}
	}
}

// anyDepUnsuccessful reports whether any name in deps maps to a step that
// terminated unsuccessfully ([store.StepStatusFailed] or
// [store.StepStatusCanceled]) in statusByName.
func anyDepUnsuccessful(deps []string, statusByName map[string]store.StepStatus) bool {
	for _, name := range deps {
		if s := statusByName[name]; s == store.StepStatusFailed || s == store.StepStatusCanceled {
			return true
		}
	}
	return false
}
