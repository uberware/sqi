// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// blockedOn creates a blocked job with one edge per upstream, written by its
// submission.
func blockedOn(t *testing.T, st store.Store, upstreams ...jobGraph) jobGraph {
	t.Helper()
	ids := make([]string, 0, len(upstreams))
	for _, up := range upstreams {
		ids = append(ids, up.Job.ID)
	}
	return blockedWith(t, st, upstreams[0], ids,
		stepSpec{name: "a", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending}})
}

// blockedWith creates a blocked job in share's farm and queue, waiting on
// upstreamIDs, with the given steps.
func blockedWith(t *testing.T, st store.Store, share jobGraph, upstreamIDs []string, specs ...stepSpec) jobGraph {
	t.Helper()
	return seedGraph(t, st, graphOpts{jobStatus: store.JobStatusBlocked, share: &share, dependsOn: upstreamIDs}, specs...)
}

// failingUpstream seeds a running job whose only step has already failed. It
// is submitted alive because a submission refuses a failed upstream; failJob
// fails it through FinalizeJob once its dependents are in.
func failingUpstream(t *testing.T, st store.Store, opts graphOpts) jobGraph {
	t.Helper()
	opts.jobStatus = store.JobStatusRunning
	return seedGraph(t, st, opts, stepSpec{name: "up", status: store.StepStatusFailed, tasks: []store.TaskStatus{store.TaskStatusFailed}})
}

// failJob finalizes a failingUpstream job, which fails it.
func failJob(t *testing.T, st store.Store, g jobGraph) {
	t.Helper()
	status, _, err := st.FinalizeJob(t.Context(), g.Job.ID, time.Now().UTC())
	if err != nil || status != store.JobStatusFailed {
		t.Fatalf("FinalizeJob = (%s, %v), want failed", status, err)
	}
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
			failed := failingUpstream(t, st, graphOpts{share: &done})
			g := blockedOn(t, st, done, failed)
			failJob(t, st, failed)
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

// TestReleaseBlockedJob_DoesNotUndoCancel pins that releasing a blocked job
// the user has already canceled leaves it canceled.
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
			up := failingUpstream(t, st, graphOpts{})
			g := blockedOn(t, st, up)
			failJob(t, st, up)
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
			up := failingUpstream(t, st, graphOpts{})
			g := blockedOn(t, st, up)
			failJob(t, st, up)
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
			up := failingUpstream(t, st, graphOpts{})
			g := blockedWith(t, st, up, []string{up.Job.ID},
				stepSpec{name: "done", status: store.StepStatusCompleted, tasks: []store.TaskStatus{store.TaskStatusSucceeded}},
				stepSpec{
					name: "open", status: store.StepStatusPending,
					tasks: []store.TaskStatus{store.TaskStatusPending}, reasons: []string{"earlier reason"},
				})
			failJob(t, st, up)
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
