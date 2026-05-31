// SPDX-License-Identifier: AGPL-3.0-only

package scheduler

// policy.go implements queue- and farm-level concurrency policy evaluation
// (task 51).
//
// Before a task is assigned to a worker the scheduler evaluates two policy
// gates in order:
//
//  1. Queue policy: the target job's queue may define a MaxConcurrentTasks
//     limit. If the number of tasks already in 'assigned' or 'running' state
//     for that queue has reached the limit, the task is deferred.
//
//  2. Farm policy: the farm itself may define a MaxConcurrentTasks limit that
//     caps total active tasks across all queues. If the farm-wide active count
//     has reached the limit, the task is deferred.
//
// A limit of 0 means unlimited for both queue and farm.
//
// Both checks are performed inside [Scheduler.tryAssign] before [pickWorker] so
// that workers are not queried unnecessarily when policy would block assignment.

import (
	"context"
	"errors"
	"fmt"

	"github.com/uberware/sqi/internal/store"
)

// errPolicyBlocked is returned by the policy gate when a queue or farm
// concurrency limit prevents assignment. The task remains ready and will be
// reconsidered on the next dispatch tick.
var errPolicyBlocked = errors.New("assignment blocked by concurrency policy")

// policyGate checks per-queue and per-farm concurrency limits for the given
// job. It returns [errPolicyBlocked] if either limit is exceeded, or nil if
// the task is allowed to proceed to worker matching.
//
// queue is the job's queue; farm is the job's farm. Both must be pre-fetched
// by the caller (tryAssign) to avoid duplicate DB round-trips.
func policyGate(
	ctx context.Context,
	st store.Store,
	job store.Job,
	queue store.Queue,
	farm store.Farm,
) error {
	// ── 1. Per-queue limit ────────────────────────────────────────────────
	if queue.MaxConcurrentTasks > 0 {
		active, err := st.CountActiveTasksInQueue(ctx, job.QueueID)
		if err != nil {
			return fmt.Errorf("policy: count active tasks in queue %q: %w", job.QueueID, err)
		}
		if active >= queue.MaxConcurrentTasks {
			return fmt.Errorf(
				"policy: queue %q at capacity (%d/%d active tasks): %w",
				queue.Name, active, queue.MaxConcurrentTasks, errPolicyBlocked,
			)
		}
	}

	// ── 2. Per-farm limit ─────────────────────────────────────────────────
	if farm.MaxConcurrentTasks > 0 {
		active, err := st.CountActiveTasksInFarm(ctx, job.FarmID)
		if err != nil {
			return fmt.Errorf("policy: count active tasks in farm %q: %w", job.FarmID, err)
		}
		if active >= farm.MaxConcurrentTasks {
			return fmt.Errorf(
				"policy: farm %q at capacity (%d/%d active tasks): %w",
				farm.Name, active, farm.MaxConcurrentTasks, errPolicyBlocked,
			)
		}
	}

	return nil
}
