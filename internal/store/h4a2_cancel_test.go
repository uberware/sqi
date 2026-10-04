// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// TestCancelJobExecution_FinalizesEveryStep pins H4a2 §3.2: after a job cancel
// every step of the job is terminal, so RetryJob's step reset (failed/canceled
// -> pending) and ResolveDependencies can revive it. v0.3.0 and H4a left the
// steps open, and a later RetryJob stranded the revived tasks in pending.
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
				// crash window (spec D5); a job cancel must still not write
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

// TestCancelJobExecution_FinalizesAStepOverMaxLimit is Review Focus 1: the
// cancel's step write is one statement over the step's tasks, never a page.
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
