// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestCancelJobExecution(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusRunning, store.TaskStatusAssigned, store.TaskStatusReady, store.TaskStatusSucceeded},
			})
			pool := seedPool(t, st, 0)
			var attempts []store.TaskAttempt
			for _, tk := range g.Tasks["a"][:2] {
				a := seedAttempt(t, st, tk, store.AttemptStatusRunning)
				seedClaim(t, st, pool.ID, a.ID)
				attempts = append(attempts, a)
			}
			active, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, time.Now().UTC())
			if err != nil {
				t.Fatalf("CancelJobExecution: %v", err)
			}
			if len(active) != 2 || active[0].AssignedWorkerID != fixtureWorkerID || active[1].AssignedWorkerID != fixtureWorkerID {
				t.Fatalf("active = %+v, want the 2 in-flight tasks with their worker", active)
			}
			for i, want := range []store.TaskStatus{store.TaskStatusCanceled, store.TaskStatusCanceled, store.TaskStatusCanceled, store.TaskStatusSucceeded} {
				if got := mustTask(t, st, g.Tasks["a"][i].ID).Status; got != want {
					t.Fatalf("task %d = %q, want %q", i, got, want)
				}
			}
			// Job-level cancel clears the worker assignment of every task it
			// cancels, as it always has.
			if got := mustTask(t, st, g.Tasks["a"][0].ID); got.AssignedWorkerID != "" || got.AssignedAt != nil {
				t.Fatalf("canceled task keeps assignment (%q, %v), want it cleared", got.AssignedWorkerID, got.AssignedAt)
			}
			for _, i := range []int{0, 1, 2} {
				if got := mustTask(t, st, g.Tasks["a"][i].ID).FailureReason; got != store.FailureReasonCanceledByUser {
					t.Fatalf("task %d failure reason = %q, want %q", i, got, store.FailureReasonCanceledByUser)
				}
			}
			if got := mustTask(t, st, g.Tasks["a"][3].ID).FailureReason; got != "" {
				t.Fatalf("succeeded task failure reason = %q, want it untouched", got)
			}
			for _, a := range attempts {
				if got := mustAttempt(t, st, a.ID); got.Status != store.AttemptStatusCanceled || got.EndedAt == nil {
					t.Fatalf("attempt = %+v, want canceled with ended_at", got)
				}
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}

			again, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, time.Now().UTC())
			if err != nil || len(again) != 0 {
				t.Fatalf("second CancelJobExecution = (%+v, %v), want (empty, nil)", again, err)
			}
		})
	}
}

// TestCancelJobExecution_ReasonIsOnlyStampedWhenEmpty pins that a more specific
// cause recorded earlier (a cascade cancel) is never clobbered.
func TestCancelJobExecution_ReasonIsOnlyStampedWhenEmpty(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusReady, store.TaskStatusReady},
			})
			if err := st.SetTaskFailureReason(t.Context(), g.Tasks["a"][0].ID, store.FailureReasonUpstreamFailed); err != nil {
				t.Fatalf("SetTaskFailureReason: %v", err)
			}
			if _, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, time.Now().UTC()); err != nil {
				t.Fatalf("CancelJobExecution: %v", err)
			}
			if got := mustTask(t, st, g.Tasks["a"][0].ID).FailureReason; got != store.FailureReasonUpstreamFailed {
				t.Fatalf("reason = %q, want the earlier %q kept", got, store.FailureReasonUpstreamFailed)
			}
			if got := mustTask(t, st, g.Tasks["a"][1].ID).FailureReason; got != store.FailureReasonCanceledByUser {
				t.Fatalf("reason = %q, want %q", got, store.FailureReasonCanceledByUser)
			}
		})
	}
}

// TestCancelJobExecution_ClosesEveryRunningAttemptOfTheJob pins I3 job-wide: a
// running attempt on a task that is already terminal (which nothing should
// leave behind, but a crash can) is closed and its claim released too, on both
// backends, while another job's attempts are left alone.
func TestCancelJobExecution_ClosesEveryRunningAttemptOfTheJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusRunning},
			})
			other := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusRunning},
			})
			pool := seedPool(t, st, 0)
			leaked := seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, leaked.ID)
			live := seedAttempt(t, st, other.Tasks["a"][0], store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, live.ID)

			if _, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, time.Now().UTC()); err != nil {
				t.Fatalf("CancelJobExecution: %v", err)
			}
			if got := mustAttempt(t, st, leaked.ID).Status; got != store.AttemptStatusCanceled {
				t.Fatalf("attempt on the job's terminal task = %q, want canceled", got)
			}
			if got := mustAttempt(t, st, live.ID).Status; got != store.AttemptStatusRunning {
				t.Fatalf("another job's attempt = %q, want it left running", got)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want only the other job's 1", n)
			}
			if got := mustTask(t, st, other.Tasks["a"][0].ID).Status; got != store.TaskStatusRunning {
				t.Fatalf("another job's task = %q, want it left running", got)
			}
		})
	}
}

func TestCancelTaskExecution(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusRunning, store.TaskStatusSucceeded, store.TaskStatusRunning},
			})
			pool := seedPool(t, st, 0)
			attempt := seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, attempt.ID)
			sibling := seedAttempt(t, st, g.Tasks["a"][2], store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, sibling.ID)

			prior, ok, err := st.CancelTaskExecution(t.Context(), g.Tasks["a"][0].ID, store.FailureReasonCanceledByUser, time.Now().UTC())
			if err != nil || !ok || prior.AssignedWorkerID != fixtureWorkerID || prior.Status != store.TaskStatusRunning {
				t.Fatalf("CancelTaskExecution = (%+v, %v, %v), want canceled with the prior running task and worker", prior, ok, err)
			}
			got := mustTask(t, st, g.Tasks["a"][0].ID)
			if got.Status != store.TaskStatusCanceled || got.FailureReason != store.FailureReasonCanceledByUser {
				t.Fatalf("task = %+v, want canceled with the user reason", got)
			}
			// Single-task cancel leaves the assignment in place, as it always
			// has, so a canceled task still shows the worker that ran it.
			if got.AssignedWorkerID != fixtureWorkerID || got.AssignedAt == nil {
				t.Fatalf("canceled task assignment = (%q, %v), want it kept", got.AssignedWorkerID, got.AssignedAt)
			}
			if a := mustAttempt(t, st, attempt.ID); a.Status != store.AttemptStatusCanceled || a.EndedAt == nil {
				t.Fatalf("attempt = %+v, want canceled with ended_at", a)
			}
			if a := mustAttempt(t, st, sibling.ID); a.Status != store.AttemptStatusRunning {
				t.Fatalf("a sibling task's attempt = %q, want it left running", a.Status)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want only the sibling's 1", n)
			}

			terminal, ok, err := st.CancelTaskExecution(t.Context(), g.Tasks["a"][1].ID, "x", time.Now().UTC())
			if err != nil || ok || terminal.Status != store.TaskStatusSucceeded {
				t.Fatalf("cancel of a terminal task = (%+v, %v, %v), want (the task, false, nil)", terminal, ok, err)
			}
			if got := mustTask(t, st, g.Tasks["a"][1].ID); got.Status != store.TaskStatusSucceeded || got.FailureReason != "" {
				t.Fatalf("terminal task = %+v, want it untouched", got)
			}

			if _, ok, err := st.CancelTaskExecution(t.Context(), "no-such-task", "x", time.Now().UTC()); !errorsIsNotFound(err) || ok {
				t.Fatalf("cancel of an unknown task = (%v, %v), want (false, ErrNotFound)", ok, err)
			}
		})
	}
}

// TestCancelTaskExecution_ReasonIsOnlyStampedWhenEmpty is the single-task twin
// of the job-level rule: an earlier cascade reason survives.
func TestCancelTaskExecution_ReasonIsOnlyStampedWhenEmpty(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusReady},
			})
			if err := st.SetTaskFailureReason(t.Context(), g.Tasks["a"][0].ID, store.FailureReasonUpstreamFailed); err != nil {
				t.Fatalf("SetTaskFailureReason: %v", err)
			}
			if _, ok, err := st.CancelTaskExecution(t.Context(), g.Tasks["a"][0].ID, store.FailureReasonCanceledByUser, time.Now().UTC()); err != nil || !ok {
				t.Fatalf("CancelTaskExecution = (%v, %v), want canceled", ok, err)
			}
			if got := mustTask(t, st, g.Tasks["a"][0].ID).FailureReason; got != store.FailureReasonUpstreamFailed {
				t.Fatalf("reason = %q, want the earlier %q kept", got, store.FailureReasonUpstreamFailed)
			}
		})
	}
}
