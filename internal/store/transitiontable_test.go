// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestJobOperations_FromStatesAreLegal pins decision D4: each named job
// operation's from-states, exactly as written in its SQL guard, must be legal
// sources for its target in store.JobTransitions. Update this table in the same
// change as any guard.
//
// A same-status write is a no-op (invariant I1), not a transition, so a
// from-state equal to the target is skipped rather than listed as an arrow:
// ParkJob's guard admits an already-paused job, and the table deliberately has
// no paused → paused entry.
//
// FinalizeJob's SQL guard is "status NOT IN (terminal)", which also admits
// blocked, and a blocked job can be finalized. Canceling each of its tasks one
// at a time leaves its pending steps with only terminal tasks, the startup
// reconcile finalizes them pending → canceled, and FinalizeJob then moves the
// job blocked → canceled: a legal arrow, listed below as its own entry.
// blocked → completed and blocked → failed cannot happen, because a blocked
// job's tasks are never leased, so none of them succeeds or fails.
func TestJobOperations_FromStatesAreLegal(t *testing.T) {
	nonTerminal := []store.JobStatus{store.JobStatusPending, store.JobStatusRunning, store.JobStatusPaused, store.JobStatusBlocked}
	finalizable := []store.JobStatus{store.JobStatusPending, store.JobStatusRunning, store.JobStatusPaused}

	type operation struct {
		op   string
		from []store.JobStatus
		to   store.JobStatus
	}
	ops := []operation{
		{"PromoteJobRunning", []store.JobStatus{store.JobStatusPending}, store.JobStatusRunning},
		{"PauseJob", []store.JobStatus{store.JobStatusPending, store.JobStatusRunning}, store.JobStatusPaused},
		{"ParkJob", nonTerminal, store.JobStatusPaused},
		{"ResumeJob", []store.JobStatus{store.JobStatusPaused}, store.JobStatusPending},
		{"ReleaseBlockedJob", []store.JobStatus{store.JobStatusBlocked}, store.JobStatusPending},
		{"CancelBlockedJob", []store.JobStatus{store.JobStatusBlocked}, store.JobStatusCanceled},
		{"CancelJobStatus", nonTerminal, store.JobStatusCanceled},
		// The job-row write at the end of the job cancel, under CancelJobStatus's guard.
		{"CancelJobExecution", nonTerminal, store.JobStatusCanceled},
		{"DemoteStalledJobs", []store.JobStatus{store.JobStatusRunning}, store.JobStatusPending},
		{"RetryTasks", []store.JobStatus{store.JobStatusFailed, store.JobStatusCanceled, store.JobStatusPaused}, store.JobStatusPending},
	}
	for _, target := range []store.JobStatus{store.JobStatusCompleted, store.JobStatusFailed, store.JobStatusCanceled} {
		ops = append(ops, operation{"FinalizeJob", finalizable, target})
	}
	// A blocked job whose tasks were all canceled one at a time (see above).
	ops = append(ops, operation{"FinalizeJob", []store.JobStatus{store.JobStatusBlocked}, store.JobStatusCanceled})

	for _, o := range ops {
		for _, f := range o.from {
			if f == o.to {
				continue
			}
			if !slices.Contains(store.JobTransitions[f], o.to) {
				t.Errorf("%s: %s → %s is not in store.JobTransitions", o.op, f, o.to)
			}
		}
	}
}
