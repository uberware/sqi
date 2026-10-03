// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// blockedOn creates a blocked job whose single edge points at upstream.
func blockedOn(t *testing.T, st store.Store, upstream h4aGraph) h4aGraph {
	t.Helper()
	g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusBlocked, share: &upstream},
		stepSpec{name: "a", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending}})
	if err := st.CreateJobDependencies(t.Context(), g.Job.ID, []string{upstream.Job.ID}); err != nil {
		t.Fatalf("CreateJobDependencies: %v", err)
	}
	return g
}

func TestReleaseBlockedJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			up := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning})
			g := blockedOn(t, st, up)
			if ok, err := st.ReleaseBlockedJob(t.Context(), g.Job.ID, time.Now().UTC()); err != nil || ok {
				t.Fatalf("release with running upstream = (%v, %v), want (false, nil)", ok, err)
			}
			if mustJob(t, st, g.Job.ID).Status != store.JobStatusBlocked {
				t.Fatal("a refused release must leave the job blocked")
			}
			if _, _, err := st.FinalizeJob(t.Context(), up.Job.ID, time.Now().UTC()); err != nil {
				t.Fatalf("FinalizeJob upstream: %v", err)
			}
			if ok, err := st.ReleaseBlockedJob(t.Context(), g.Job.ID, time.Now().UTC()); err != nil || !ok {
				t.Fatalf("release with completed upstream = (%v, %v), want (true, nil)", ok, err)
			}
			if mustJob(t, st, g.Job.ID).Status != store.JobStatusPending {
				t.Fatal("job not pending")
			}
			// The job is no longer blocked, so a second release is a no-op.
			if ok, err := st.ReleaseBlockedJob(t.Context(), g.Job.ID, time.Now().UTC()); err != nil || ok {
				t.Fatalf("second release = (%v, %v), want (false, nil)", ok, err)
			}
		})
	}
}

// TestReleaseBlockedJob_RequiresEveryUpstream pins that one completed upstream
// is not enough when a second one is not, and that a deleted upstream (its
// edge survives deletion on purpose) never counts as satisfied.
func TestReleaseBlockedJob_RequiresEveryUpstream(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			done := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCompleted})
			failed := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusFailed, share: &done})
			g := blockedOn(t, st, done)
			if err := st.CreateJobDependencies(t.Context(), g.Job.ID, []string{failed.Job.ID}); err != nil {
				t.Fatalf("CreateJobDependencies: %v", err)
			}
			if ok, err := st.ReleaseBlockedJob(t.Context(), g.Job.ID, time.Now().UTC()); err != nil || ok {
				t.Fatalf("release with one failed upstream = (%v, %v), want (false, nil)", ok, err)
			}

			// A deleted upstream leaves its edge behind and must not satisfy it.
			gone := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCompleted, share: &done})
			h := blockedOn(t, st, gone)
			if err := st.DeleteJob(t.Context(), gone.Job.ID); err != nil {
				t.Fatalf("DeleteJob upstream: %v", err)
			}
			if ok, err := st.ReleaseBlockedJob(t.Context(), h.Job.ID, time.Now().UTC()); err != nil || ok {
				t.Fatalf("release with deleted upstream = (%v, %v), want (false, nil)", ok, err)
			}
			if mustJob(t, st, h.Job.ID).Status != store.JobStatusBlocked {
				t.Fatal("a refused release must leave the job blocked")
			}
		})
	}
}

// TestReleaseBlockedJob_DoesNotUndoCancel pins F9 at the store layer.
func TestReleaseBlockedJob_DoesNotUndoCancel(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			up := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCompleted})
			g := blockedOn(t, st, up)
			if err := st.CancelJobStatus(t.Context(), g.Job.ID); err != nil {
				t.Fatalf("CancelJobStatus: %v", err)
			}
			if ok, err := st.ReleaseBlockedJob(t.Context(), g.Job.ID, time.Now().UTC()); err != nil || ok {
				t.Fatalf("release after user cancel = (%v, %v), want (false, nil)", ok, err)
			}
			if mustJob(t, st, g.Job.ID).Status != store.JobStatusCanceled {
				t.Fatal("user cancel was overwritten")
			}
		})
	}
}

func TestCancelBlockedJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			up := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusFailed})
			g := blockedOn(t, st, up)
			ok, tasks, err := st.CancelBlockedJob(t.Context(), g.Job.ID, store.FailureReasonUpstreamFailed, time.Now().UTC())
			if err != nil || !ok || len(tasks) != 1 {
				t.Fatalf("CancelBlockedJob = (%v, %d, %v), want (true, 1, nil)", ok, len(tasks), err)
			}
			if mustJob(t, st, g.Job.ID).Status != store.JobStatusCanceled ||
				mustStep(t, st, g.Steps["a"].ID).Status != store.StepStatusCanceled ||
				mustTask(t, st, g.Tasks["a"][0].ID).Status != store.TaskStatusCanceled {
				t.Fatal("job, step and task must all be canceled")
			}
			if got := mustTask(t, st, g.Tasks["a"][0].ID).FailureReason; got != store.FailureReasonUpstreamFailed {
				t.Fatalf("task failure reason = %q, want %q", got, store.FailureReasonUpstreamFailed)
			}
			if ok, _, err := st.CancelBlockedJob(t.Context(), g.Job.ID, "x", time.Now().UTC()); err != nil || ok {
				t.Fatalf("second CancelBlockedJob = (%v, %v), want (false, nil)", ok, err)
			}
		})
	}
}

// TestCancelBlockedJob_WritesNothingWhenNotBlocked pins the guard: a job that
// is no longer blocked keeps its steps and tasks exactly as they were.
func TestCancelBlockedJob_WritesNothingWhenNotBlocked(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			up := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusFailed})
			g := blockedOn(t, st, up)
			if err := st.CancelJobStatus(t.Context(), g.Job.ID); err != nil {
				t.Fatalf("CancelJobStatus: %v", err)
			}
			ok, tasks, err := st.CancelBlockedJob(t.Context(), g.Job.ID, store.FailureReasonUpstreamFailed, time.Now().UTC())
			if err != nil || ok || len(tasks) != 0 {
				t.Fatalf("CancelBlockedJob on a canceled job = (%v, %d, %v), want (false, 0, nil)", ok, len(tasks), err)
			}
			if got := mustStep(t, st, g.Steps["a"].ID).Status; got != store.StepStatusPending {
				t.Fatalf("step = %q, want pending (nothing may be written)", got)
			}
			task := mustTask(t, st, g.Tasks["a"][0].ID)
			if task.Status != store.TaskStatusPending || task.FailureReason != "" {
				t.Fatalf("task = (%q, %q), want (pending, no reason)", task.Status, task.FailureReason)
			}
		})
	}
}

// TestCancelBlockedJob_LeavesTerminalStepsAndKeepsReason pins that a step that
// already finished is not rewritten and that a reason a task already carries
// is not replaced.
func TestCancelBlockedJob_LeavesTerminalStepsAndKeepsReason(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			up := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusFailed})
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusBlocked, share: &up},
				stepSpec{name: "done", status: store.StepStatusCompleted, tasks: []store.TaskStatus{store.TaskStatusSucceeded}},
				stepSpec{name: "open", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending}})
			if err := st.CreateJobDependencies(t.Context(), g.Job.ID, []string{up.Job.ID}); err != nil {
				t.Fatalf("CreateJobDependencies: %v", err)
			}
			if err := st.SetTaskFailureReason(t.Context(), g.Tasks["open"][0].ID, "earlier reason"); err != nil {
				t.Fatalf("SetTaskFailureReason: %v", err)
			}
			ok, tasks, err := st.CancelBlockedJob(t.Context(), g.Job.ID, store.FailureReasonUpstreamFailed, time.Now().UTC())
			if err != nil || !ok || len(tasks) != 1 {
				t.Fatalf("CancelBlockedJob = (%v, %d, %v), want (true, 1, nil)", ok, len(tasks), err)
			}
			if got := mustStep(t, st, g.Steps["done"].ID).Status; got != store.StepStatusCompleted {
				t.Fatalf("completed step = %q, want completed", got)
			}
			if got := mustStep(t, st, g.Steps["open"].ID).Status; got != store.StepStatusCanceled {
				t.Fatalf("open step = %q, want canceled", got)
			}
			if got := mustTask(t, st, g.Tasks["done"][0].ID).Status; got != store.TaskStatusSucceeded {
				t.Fatalf("succeeded task = %q, want succeeded", got)
			}
			if got := mustTask(t, st, g.Tasks["open"][0].ID).FailureReason; got != "earlier reason" {
				t.Fatalf("task failure reason = %q, want the earlier one preserved", got)
			}
		})
	}
}

func TestBlockedJobOperations_UnknownJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := st.ReleaseBlockedJob(t.Context(), "nope", time.Now().UTC()); !errorsIsNotFound(err) {
				t.Fatalf("ReleaseBlockedJob(unknown) err = %v, want ErrNotFound", err)
			}
			if _, _, err := st.CancelBlockedJob(t.Context(), "nope", "x", time.Now().UTC()); !errorsIsNotFound(err) {
				t.Fatalf("CancelBlockedJob(unknown) err = %v, want ErrNotFound", err)
			}
		})
	}
}
