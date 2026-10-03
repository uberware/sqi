// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestReleaseStep(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusPending,
				tasks: []store.TaskStatus{store.TaskStatusPending, store.TaskStatusPending},
			})
			ok, tasks, err := st.ReleaseStep(t.Context(), g.Steps["a"].ID, time.Now().UTC())
			if err != nil || !ok || len(tasks) != 2 {
				t.Fatalf("ReleaseStep = (%v, %d tasks, %v), want (true, 2, nil)", ok, len(tasks), err)
			}
			if mustStep(t, st, g.Steps["a"].ID).Status != store.StepStatusReady {
				t.Fatal("step not ready")
			}
			for _, tk := range g.Tasks["a"] {
				if mustTask(t, st, tk.ID).Status != store.TaskStatusReady {
					t.Fatal("task not ready")
				}
			}
			// Precondition failure: the step is no longer pending.
			ok, tasks, err = st.ReleaseStep(t.Context(), g.Steps["a"].ID, time.Now().UTC())
			if err != nil || ok || len(tasks) != 0 {
				t.Fatalf("second ReleaseStep = (%v, %d, %v), want (false, 0, nil)", ok, len(tasks), err)
			}
		})
	}
}

func TestReleaseStep_CanceledStepIsNotRevived(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusCanceled,
				tasks: []store.TaskStatus{store.TaskStatusCanceled},
			})
			ok, _, err := st.ReleaseStep(t.Context(), g.Steps["a"].ID, time.Now().UTC())
			if err != nil || ok {
				t.Fatalf("ReleaseStep(canceled) = (%v, %v), want (false, nil)", ok, err)
			}
			if mustStep(t, st, g.Steps["a"].ID).Status != store.StepStatusCanceled {
				t.Fatal("canceled step was changed")
			}
			if mustTask(t, st, g.Tasks["a"][0].ID).Status != store.TaskStatusCanceled {
				t.Fatal("canceled task was changed")
			}
		})
	}
}

// A step that is pending but whose tasks are not all pending releases only the
// pending ones: the guard is on the step, and the task move is on pending tasks.
func TestReleaseStep_LeavesNonPendingTasksAlone(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusPending,
				tasks: []store.TaskStatus{store.TaskStatusPending, store.TaskStatusCanceled},
			})
			ok, tasks, err := st.ReleaseStep(t.Context(), g.Steps["a"].ID, time.Now().UTC())
			if err != nil || !ok || len(tasks) != 1 {
				t.Fatalf("ReleaseStep = (%v, %d tasks, %v), want (true, 1, nil)", ok, len(tasks), err)
			}
			if tasks[0].ID != g.Tasks["a"][0].ID || tasks[0].Status != store.TaskStatusReady {
				t.Fatalf("promoted task = %s/%q, want %s/ready", tasks[0].ID, tasks[0].Status, g.Tasks["a"][0].ID)
			}
			if mustTask(t, st, g.Tasks["a"][1].ID).Status != store.TaskStatusCanceled {
				t.Fatal("non-pending task was changed")
			}
		})
	}
}

func TestReleaseStep_UnknownStep(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ok, tasks, err := st.ReleaseStep(t.Context(), "no-such-step", time.Now().UTC())
			if !errors.Is(err, store.ErrNotFound) || ok || len(tasks) != 0 {
				t.Fatalf("ReleaseStep(unknown) = (%v, %d, %v), want (false, 0, ErrNotFound)", ok, len(tasks), err)
			}
		})
	}
}

func TestCancelPendingStep(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusPending,
				tasks: []store.TaskStatus{store.TaskStatusPending},
			})
			ok, tasks, err := st.CancelPendingStep(t.Context(), g.Steps["a"].ID, store.FailureReasonUpstreamFailed, time.Now().UTC())
			if err != nil || !ok || len(tasks) != 1 {
				t.Fatalf("CancelPendingStep = (%v, %d, %v), want (true, 1, nil)", ok, len(tasks), err)
			}
			if mustStep(t, st, g.Steps["a"].ID).Status != store.StepStatusCanceled {
				t.Fatal("step not canceled")
			}
			got := mustTask(t, st, g.Tasks["a"][0].ID)
			if got.Status != store.TaskStatusCanceled || got.FailureReason != store.FailureReasonUpstreamFailed {
				t.Fatalf("task = %q/%q, want canceled/upstream-failed", got.Status, got.FailureReason)
			}
			ok, _, err = st.CancelPendingStep(t.Context(), g.Steps["a"].ID, "x", time.Now().UTC())
			if err != nil || ok {
				t.Fatalf("second CancelPendingStep = (%v, %v), want (false, nil)", ok, err)
			}
		})
	}
}

// A step that is no longer pending (another writer moved it) is not
// overwritten, and its tasks are not touched.
func TestCancelPendingStep_MovedStepIsNotOverwritten(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusReady},
			})
			ok, tasks, err := st.CancelPendingStep(t.Context(), g.Steps["a"].ID, store.FailureReasonUpstreamFailed, time.Now().UTC())
			if err != nil || ok || len(tasks) != 0 {
				t.Fatalf("CancelPendingStep(ready) = (%v, %d, %v), want (false, 0, nil)", ok, len(tasks), err)
			}
			if mustStep(t, st, g.Steps["a"].ID).Status != store.StepStatusReady {
				t.Fatal("ready step was changed")
			}
			if got := mustTask(t, st, g.Tasks["a"][0].ID); got.Status != store.TaskStatusReady || got.FailureReason != "" {
				t.Fatalf("task = %q/%q, want ready with no reason", got.Status, got.FailureReason)
			}
		})
	}
}

func TestCancelPendingStep_KeepsMoreSpecificReason(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusPending,
				tasks: []store.TaskStatus{store.TaskStatusPending, store.TaskStatusPending},
			})
			const specific = "the earlier, more specific cause"
			if err := st.SetTaskFailureReason(t.Context(), g.Tasks["a"][0].ID, specific); err != nil {
				t.Fatalf("SetTaskFailureReason: %v", err)
			}
			ok, tasks, err := st.CancelPendingStep(t.Context(), g.Steps["a"].ID, store.FailureReasonUpstreamFailed, time.Now().UTC())
			if err != nil || !ok || len(tasks) != 2 {
				t.Fatalf("CancelPendingStep = (%v, %d, %v), want (true, 2, nil)", ok, len(tasks), err)
			}
			if got := mustTask(t, st, g.Tasks["a"][0].ID).FailureReason; got != specific {
				t.Fatalf("reason = %q, want the specific one kept", got)
			}
			if got := mustTask(t, st, g.Tasks["a"][1].ID).FailureReason; got != store.FailureReasonUpstreamFailed {
				t.Fatalf("reason = %q, want %q", got, store.FailureReasonUpstreamFailed)
			}
		})
	}
}

func TestCancelPendingStep_UnknownStep(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ok, tasks, err := st.CancelPendingStep(t.Context(), "no-such-step", "x", time.Now().UTC())
			if !errors.Is(err, store.ErrNotFound) || ok || len(tasks) != 0 {
				t.Fatalf("CancelPendingStep(unknown) = (%v, %d, %v), want (false, 0, ErrNotFound)", ok, len(tasks), err)
			}
		})
	}
}
