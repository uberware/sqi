// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// TestCancelJobExecution_FinalizesEveryStep pins that after a job cancel every
// step of the job is terminal, so RetryJob's step reset (failed/canceled ->
// pending) and ResolveDependencies can revive it. A step left open would
// strand a later RetryJob's revived tasks in pending.
func TestCancelJobExecution_FinalizesEveryStep(t *testing.T) {
	S, F, C, R, P, A := store.TaskStatusSucceeded, store.TaskStatusFailed, store.TaskStatusCanceled,
		store.TaskStatusRunning, store.TaskStatusPending, store.TaskStatusReady
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(
				t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "done", status: store.StepStatusReady, tasks: []store.TaskStatus{S, S}},
				stepSpec{name: "mid", status: store.StepStatusReady, tasks: []store.TaskStatus{S, R}},
				stepSpec{name: "bad", status: store.StepStatusReady, tasks: []store.TaskStatus{F, A}},
				stepSpec{name: "later", status: store.StepStatusPending, dependsOn: []string{"mid"}, tasks: []store.TaskStatus{P}},
				// A pending step holding a failed task exists only in the retry
				// crash window; a job cancel must still not write
				// pending -> failed, which the step table does not allow.
				stepSpec{name: "window", status: store.StepStatusPending, tasks: []store.TaskStatus{F, P}},
				stepSpec{name: "empty", status: store.StepStatusReady},
				stepSpec{name: "already", status: store.StepStatusCompleted, tasks: []store.TaskStatus{S}},
				stepSpec{name: "prior", status: store.StepStatusCanceled, tasks: []store.TaskStatus{C}},
			)
			seedAttempt(t, st, g.Tasks["mid"][1], store.AttemptStatusRunning)

			if _, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, time.Now().UTC()); err != nil {
				t.Fatalf("CancelJobExecution: %v", err)
			}
			want := map[string]store.StepStatus{
				"done": store.StepStatusCompleted, "mid": store.StepStatusCanceled, "bad": store.StepStatusFailed,
				"later": store.StepStatusCanceled, "window": store.StepStatusCanceled, "empty": store.StepStatusCanceled,
				"already": store.StepStatusCompleted, "prior": store.StepStatusCanceled,
			}
			for stepName, w := range want {
				if got := mustStep(t, st, g.Steps[stepName].ID).Status; got != w {
					t.Errorf("step %s = %q, want %q", stepName, got, w)
				}
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// TestCancelJobExecution_CancelsTheJobRow pins that the job's own row is
// canceled in the transaction that cancels its work, whatever non-terminal
// status it held. Before, the job row was a second write (CancelJobStatus) whose
// loss left a job with every step terminal and nothing for a start to repair.
func TestCancelJobExecution_CancelsTheJobRow(t *testing.T) {
	for _, status := range []store.JobStatus{store.JobStatusRunning, store.JobStatusPending, store.JobStatusBlocked} {
		for name, st := range newStores(t) {
			t.Run(string(status)+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{jobStatus: status},
					stepSpec{name: "a", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending}})
				now := time.Now().UTC().Round(0)
				if _, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, now); err != nil {
					t.Fatalf("CancelJobExecution: %v", err)
				}
				got := mustJob(t, st, g.Job.ID)
				if got.Status != store.JobStatusCanceled {
					t.Fatalf("job = %q, want canceled", got.Status)
				}
				if got.CompletedAt == nil || !got.CompletedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
					t.Fatalf("job completed_at = %v, updated_at = %v, want both %v", got.CompletedAt, got.UpdatedAt, now)
				}
			})
		}
	}
}

// TestCancelJobExecution_LeavesATerminalJobRowAlone pins the job-row guard: a
// completed or failed job is never overwritten, and an already canceled job is
// not re-stamped by a second cancel. CancelJobStatus after the cancel is then the
// idempotent confirmation the REST handler relies on.
func TestCancelJobExecution_LeavesATerminalJobRowAlone(t *testing.T) {
	for _, status := range []store.JobStatus{store.JobStatusCompleted, store.JobStatusFailed} {
		for name, st := range newStores(t) {
			t.Run(string(status)+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{jobStatus: status},
					stepSpec{name: "a", status: store.StepStatusCompleted, tasks: []store.TaskStatus{store.TaskStatusSucceeded}})
				before := mustJob(t, st, g.Job.ID)
				if _, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, time.Now().UTC()); err != nil {
					t.Fatalf("CancelJobExecution: %v", err)
				}
				after := mustJob(t, st, g.Job.ID)
				if after.Status != status || !after.UpdatedAt.Equal(before.UpdatedAt) || (after.CompletedAt == nil) != (before.CompletedAt == nil) {
					t.Fatalf("job = %+v, want it untouched (%+v)", after, before)
				}
				if err := st.CancelJobStatus(t.Context(), g.Job.ID); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("CancelJobStatus = %v, want ErrConflict", err)
				}
			})
		}
	}
	for name, st := range newStores(t) {
		t.Run("canceled/"+name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})
			first := time.Now().UTC().Add(-time.Minute).Round(0)
			if _, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, first); err != nil {
				t.Fatalf("CancelJobExecution: %v", err)
			}
			if _, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, time.Now().UTC()); err != nil {
				t.Fatalf("second CancelJobExecution: %v", err)
			}
			if err := st.CancelJobStatus(t.Context(), g.Job.ID); err != nil {
				t.Fatalf("CancelJobStatus after the cancel = %v, want nil (idempotent)", err)
			}
			got := mustJob(t, st, g.Job.ID)
			if got.Status != store.JobStatusCanceled || got.CompletedAt == nil || !got.CompletedAt.Equal(first) || !got.UpdatedAt.Equal(first) {
				t.Fatalf("job = %q completed_at %v updated_at %v, want canceled stamped once at %v", got.Status, got.CompletedAt, got.UpdatedAt, first)
			}
		})
	}
}

// TestCancelJobExecution_FinalizesAStepOverMaxLimit pins that the cancel's
// step write is one statement over the step's tasks, never a page.
func TestCancelJobExecution_FinalizesAStepOverMaxLimit(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "big", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusSucceeded}})
			now := time.Now().UTC()
			for range store.MaxLimit {
				if _, err := st.CreateTask(t.Context(), store.Task{
					ID: uuid.NewString(), JobID: g.Job.ID, StepID: g.Steps["big"].ID, Name: "t",
					Status: store.TaskStatusReady, CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					t.Fatalf("CreateTask: %v", err)
				}
			}
			if _, err := st.CancelJobExecution(t.Context(), g.Job.ID, store.FailureReasonCanceledByUser, now); err != nil {
				t.Fatalf("CancelJobExecution: %v", err)
			}
			if got := mustStep(t, st, g.Steps["big"].ID).Status; got != store.StepStatusCanceled {
				t.Fatalf("step = %q, want canceled", got)
			}
		})
	}
}
