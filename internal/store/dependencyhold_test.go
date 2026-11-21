// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// submitPausedHeld submits a job straight into paused while still holding its
// dependency on upstream, the way openjd.Submitter writes a paused submission
// whose upstream has not completed. Its one step is "a", as blockedOn's is.
func submitPausedHeld(t *testing.T, st store.Store, upstream jobGraph) store.Job {
	t.Helper()
	sub := graphSubmission(jobGraph{Farm: upstream.Farm, Queue: upstream.Queue},
		graphOpts{dependsOn: []string{upstream.Job.ID}},
		[]stepSpec{{name: "a", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending}}})
	sub.Job.Status, sub.Job.DependencyHold = store.JobStatusPaused, true
	return storetest.Submit(t, st, sub).Job
}

// mustPause pauses jobID or fails the test.
func mustPause(t *testing.T, st store.Store, jobID string) {
	t.Helper()
	if err := st.PauseJob(t.Context(), jobID, time.Now().UTC()); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
}

// mustResume resumes jobID or fails the test.
func mustResume(t *testing.T, st store.Store, jobID string) {
	t.Helper()
	if err := st.ResumeJob(t.Context(), jobID, time.Now().UTC()); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
}

func TestCreateJobSubmission_DependencyHoldRoundTrips(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			up := seedGraph(t, st, graphOpts{})
			if got := mustJob(t, st, submitPausedHeld(t, st, up).ID); got.Status != store.JobStatusPaused || !got.DependencyHold {
				t.Fatalf("held job = %q hold=%v, want paused with hold", got.Status, got.DependencyHold)
			}
			// The submission leaves DependencyHold unset: the store writes it for
			// every job created blocked.
			if !mustJob(t, st, blockedOn(t, st, up).Job.ID).DependencyHold {
				t.Fatal("blocked job has no dependency hold")
			}
			if mustJob(t, st, up.Job.ID).DependencyHold {
				t.Fatal("pending job with no dependencies has a dependency hold")
			}
		})
	}
}

func TestResumeJob_HeldJobReturnsToBlocked(t *testing.T) {
	cases := []struct {
		name string
		held func(t *testing.T, st store.Store, up jobGraph) string // returns a paused, held job
	}{
		{"submitted paused", func(t *testing.T, st store.Store, up jobGraph) string {
			return submitPausedHeld(t, st, up).ID
		}},
		{"paused while blocked", func(t *testing.T, st store.Store, up jobGraph) string {
			id := blockedOn(t, st, up).Job.ID
			mustPause(t, st, id)
			return id
		}},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				id := tc.held(t, st, seedGraph(t, st, graphOpts{}))
				mustResume(t, st, id)
				if got := mustJob(t, st, id); got.Status != store.JobStatusBlocked || !got.DependencyHold {
					t.Fatalf("resumed held job = %q hold=%v, want blocked with hold", got.Status, got.DependencyHold)
				}
			})
		}
	}
}

// A job whose hold was released must never return to blocked, even once its
// upstream is gone: blocked would make the dependency sweep cancel it for a
// missing upstream it no longer waits on.
func TestReleaseBlockedJob_ClearsDependencyHold(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			up := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCompleted})
			dep := blockedOn(t, st, up)
			released, err := st.ReleaseBlockedJob(ctx, dep.Job.ID, time.Now().UTC())
			if err != nil || !released {
				t.Fatalf("ReleaseBlockedJob = %v, %v; want true, nil", released, err)
			}
			if mustJob(t, st, dep.Job.ID).DependencyHold {
				t.Fatal("released job still holds its dependency")
			}
			mustPause(t, st, dep.Job.ID)
			if err := st.DeleteJob(ctx, up.Job.ID); err != nil {
				t.Fatalf("DeleteJob upstream: %v", err)
			}
			mustResume(t, st, dep.Job.ID)
			if got := mustJob(t, st, dep.Job.ID).Status; got != store.JobStatusPending {
				t.Fatalf("resumed released job = %q, want pending", got)
			}
		})
	}
}

// A held job's steps are released only by the dependency release, never by
// a step-level ReleaseStep, whatever the job's status.
func TestReleaseStep_DeclinesWhileJobHoldsDependencies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paused bool
	}{{"blocked", false}, {"paused", true}} {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				dep := blockedOn(t, st, seedGraph(t, st, graphOpts{}))
				if tc.paused {
					mustPause(t, st, dep.Job.ID)
				}
				released, _, err := st.ReleaseStep(t.Context(), dep.Steps["a"].ID, time.Now().UTC())
				if err != nil || released {
					t.Fatalf("ReleaseStep = %v, %v; want false, nil", released, err)
				}
				if got := mustTask(t, st, dep.Tasks["a"][0].ID).Status; got != store.TaskStatusPending {
					t.Fatalf("task = %q, want pending", got)
				}
			})
		}
	}
}

func TestListJobIDsWithPendingSteps_SkipsHeldJobs(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			dep := blockedOn(t, st, seedGraph(t, st, graphOpts{}))
			mustPause(t, st, dep.Job.ID)
			ids, err := st.ListJobIDsWithPendingSteps(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(ids, dep.Job.ID) {
				t.Fatalf("ListJobIDsWithPendingSteps = %v, includes the paused held job", ids)
			}
		})
	}
}
