// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"fmt"
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

// failedUpstream is a failed step for a cascade-cancel test's step "a" to
// depend on: CancelPendingStep cancels only a step one of whose upstreams is
// failed or canceled when the write runs.
var failedUpstream = stepSpec{name: "up", status: store.StepStatusFailed, tasks: []store.TaskStatus{store.TaskStatusFailed}}

func TestCancelPendingStep(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, failedUpstream, stepSpec{
				name: "a", status: store.StepStatusPending, dependsOn: []string{"up"},
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
			g := seedGraph(t, st, graphOpts{}, failedUpstream, stepSpec{
				name: "a", status: store.StepStatusReady, dependsOn: []string{"up"},
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
			const specific = "the earlier, more specific cause"
			g := seedGraph(t, st, graphOpts{}, failedUpstream, stepSpec{
				name: "a", status: store.StepStatusPending, dependsOn: []string{"up"},
				tasks:   []store.TaskStatus{store.TaskStatusPending, store.TaskStatusPending},
				reasons: []string{specific},
			})
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

// The cascade decides inside its own write (invariant I4): a pending step is
// canceled only while one of its upstream steps is failed or canceled. A retry
// that revives the failed upstream between the caller's read of the step list
// and this write leaves the step pending, so it runs once the upstream
// completes instead of being canceled for a failure that no longer stands.
func TestCancelPendingStep_RequiresAnUnsuccessfulUpstream(t *testing.T) {
	cases := []struct {
		upstream   []store.StepStatus // one upstream step per entry, "u0", "u1", ...
		wantCancel bool
	}{
		{[]store.StepStatus{store.StepStatusFailed}, true},
		{[]store.StepStatus{store.StepStatusCanceled}, true},
		{[]store.StepStatus{store.StepStatusCompleted, store.StepStatusFailed}, true},
		{[]store.StepStatus{store.StepStatusReady}, false}, // a retry revived it
		{[]store.StepStatus{store.StepStatusPending}, false},
		{[]store.StepStatus{store.StepStatusRunning}, false},
		{[]store.StepStatus{store.StepStatusCompleted}, false},
		{nil, false}, // no upstream at all
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(fmt.Sprintf("%v/%s", tc.upstream, name), func(t *testing.T) {
				var specs []stepSpec
				var deps []string
				for i, status := range tc.upstream {
					up := fmt.Sprintf("u%d", i)
					specs = append(specs, stepSpec{name: up, status: status})
					deps = append(deps, up)
				}
				specs = append(specs, stepSpec{
					name: "a", status: store.StepStatusPending, dependsOn: deps,
					tasks: []store.TaskStatus{store.TaskStatusPending},
				})
				g := seedGraph(t, st, graphOpts{}, specs...)

				ok, tasks, err := st.CancelPendingStep(t.Context(), g.Steps["a"].ID, store.FailureReasonUpstreamFailed, time.Now().UTC())
				if err != nil {
					t.Fatalf("CancelPendingStep: %v", err)
				}
				if ok != tc.wantCancel || (len(tasks) == 1) != tc.wantCancel {
					t.Fatalf("CancelPendingStep = (%v, %d tasks), want canceled=%v", ok, len(tasks), tc.wantCancel)
				}
				wantStep, wantTask, wantReason := store.StepStatusPending, store.TaskStatusPending, ""
				if tc.wantCancel {
					wantStep, wantTask, wantReason = store.StepStatusCanceled, store.TaskStatusCanceled, store.FailureReasonUpstreamFailed
				}
				if got := mustStep(t, st, g.Steps["a"].ID).Status; got != wantStep {
					t.Fatalf("step = %q, want %q", got, wantStep)
				}
				if got := mustTask(t, st, g.Tasks["a"][0].ID); got.Status != wantTask || got.FailureReason != wantReason {
					t.Fatalf("task = %q/%q, want %q/%q", got.Status, got.FailureReason, wantTask, wantReason)
				}
			})
		}
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
