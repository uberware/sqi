// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestFinalizeStep_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		tasks   []store.TaskStatus
		want    store.StepStatus
		changed bool
	}{
		{"all succeeded", []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusSucceeded}, store.StepStatusCompleted, true},
		{"one failed", []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusFailed, store.TaskStatusCanceled}, store.StepStatusFailed, true},
		{"one canceled", []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusCanceled}, store.StepStatusCanceled, true},
		{"still running", []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusRunning}, "", false},
		{"retried pending", []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusPending}, "", false},
		{"zero tasks completes as today", nil, store.StepStatusCompleted, true},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
					stepSpec{name: "a", status: store.StepStatusReady, tasks: tc.tasks})
				got, changed, err := st.FinalizeStep(t.Context(), g.Steps["a"].ID, time.Now().UTC())
				if err != nil {
					t.Fatalf("FinalizeStep: %v", err)
				}
				if got != tc.want || changed != tc.changed {
					t.Fatalf("FinalizeStep = (%q, %v), want (%q, %v)", got, changed, tc.want, tc.changed)
				}
				if tc.changed && mustStep(t, st, g.Steps["a"].ID).Status != tc.want {
					t.Fatalf("step row not written")
				}
				if !tc.changed && mustStep(t, st, g.Steps["a"].ID).Status != store.StepStatusReady {
					t.Fatalf("step row written although nothing was finalized")
				}
			})
		}
	}
}

// TestFinalizeStep_AlreadyTerminal pins Review Focus #2: a redelivery after a
// committed finalize gets the terminal status back with changed=false, so the
// caller still runs (idempotent) propagation.
func TestFinalizeStep_AlreadyTerminal(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusFailed,
				tasks: []store.TaskStatus{store.TaskStatusFailed},
			})
			got, changed, err := st.FinalizeStep(t.Context(), g.Steps["a"].ID, time.Now().UTC())
			if err != nil || got != store.StepStatusFailed || changed {
				t.Fatalf("FinalizeStep = (%q, %v, %v), want (failed, false, nil)", got, changed, err)
			}
		})
	}
}

// TestFinalizeStep_MoreThanMaxLimitTasks pins F6 at the store layer. The
// decisive task is the LAST one, index MaxLimit, so it lies beyond the first
// page: an implementation that reads only MaxLimit tasks sees all-succeeded and
// reports the wrong outcome. The all-succeeded case is the plain regression
// guard and cannot tell a paged read from a full one on its own.
func TestFinalizeStep_MoreThanMaxLimitTasks(t *testing.T) {
	cases := []struct {
		name    string
		last    store.TaskStatus
		want    store.StepStatus
		changed bool
	}{
		{"all succeeded", store.TaskStatusSucceeded, store.StepStatusCompleted, true},
		{"failed beyond the first page", store.TaskStatusFailed, store.StepStatusFailed, true},
		{"running beyond the first page", store.TaskStatusRunning, "", false},
	}
	for _, tc := range cases {
		tasks := make([]store.TaskStatus, store.MaxLimit+1)
		for i := range tasks {
			tasks[i] = store.TaskStatusSucceeded
		}
		tasks[store.MaxLimit] = tc.last
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{}, stepSpec{name: "a", status: store.StepStatusReady, tasks: tasks})
				got, changed, err := st.FinalizeStep(t.Context(), g.Steps["a"].ID, time.Now().UTC())
				if err != nil || got != tc.want || changed != tc.changed {
					t.Fatalf("FinalizeStep over %d tasks = (%q, %v, %v), want (%q, %v, nil)",
						len(tasks), got, changed, err, tc.want, tc.changed)
				}
				wantRow := store.StepStatusReady
				if tc.changed {
					wantRow = tc.want
				}
				if row := mustStep(t, st, g.Steps["a"].ID).Status; row != wantRow {
					t.Fatalf("step row = %q, want %q", row, wantRow)
				}
			})
		}
	}
}

func TestFinalizeStep_NotFound(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if _, _, err := st.FinalizeStep(t.Context(), "nope", time.Now().UTC()); err == nil || !errorsIsNotFound(err) {
				t.Fatalf("FinalizeStep(unknown) err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestFinalizeJob_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		steps   []store.StepStatus
		start   store.JobStatus
		want    store.JobStatus
		changed bool
	}{
		{"all completed", []store.StepStatus{store.StepStatusCompleted, store.StepStatusCompleted}, store.JobStatusRunning, store.JobStatusCompleted, true},
		{"one failed", []store.StepStatus{store.StepStatusCompleted, store.StepStatusFailed}, store.JobStatusRunning, store.JobStatusFailed, true},
		{"one canceled", []store.StepStatus{store.StepStatusCompleted, store.StepStatusCanceled}, store.JobStatusRunning, store.JobStatusCanceled, true},
		{"step pending", []store.StepStatus{store.StepStatusCompleted, store.StepStatusPending}, store.JobStatusRunning, "", false},
		{"paused job still finalizes, as today", []store.StepStatus{store.StepStatusCompleted}, store.JobStatusPaused, store.JobStatusCompleted, true},
		{"already terminal", []store.StepStatus{store.StepStatusCompleted}, store.JobStatusCompleted, store.JobStatusCompleted, false},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				specs := make([]stepSpec, len(tc.steps))
				for i, s := range tc.steps {
					specs[i] = stepSpec{name: string(rune('a' + i)), status: s}
				}
				g := seedGraph(t, st, graphOpts{jobStatus: tc.start}, specs...)
				got, changed, err := st.FinalizeJob(t.Context(), g.Job.ID, time.Now().UTC())
				if err != nil {
					t.Fatalf("FinalizeJob: %v", err)
				}
				if got != tc.want || changed != tc.changed {
					t.Fatalf("FinalizeJob = (%q, %v), want (%q, %v)", got, changed, tc.want, tc.changed)
				}
				if tc.changed {
					j := mustJob(t, st, g.Job.ID)
					if j.Status != tc.want || j.CompletedAt == nil {
						t.Fatalf("job row = %q completed_at=%v, want %q with completed_at set", j.Status, j.CompletedAt, tc.want)
					}
				}
			})
		}
	}
}

func TestFinalizeJob_NotFound(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if _, _, err := st.FinalizeJob(t.Context(), "nope", time.Now().UTC()); err == nil || !errorsIsNotFound(err) {
				t.Fatalf("FinalizeJob(unknown) err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestListStuckSteps pins Review Focus #5: zero-task steps are not stuck.
func TestListStuckSteps(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(
				t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "stuck", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusSucceeded}},
				stepSpec{name: "live", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}},
				stepSpec{name: "empty", status: store.StepStatusReady},
				stepSpec{name: "done", status: store.StepStatusCompleted, tasks: []store.TaskStatus{store.TaskStatusSucceeded}},
			)
			got, err := st.ListStuckSteps(t.Context())
			if err != nil {
				t.Fatalf("ListStuckSteps: %v", err)
			}
			if len(got) != 1 || got[0].ID != g.Steps["stuck"].ID {
				t.Fatalf("ListStuckSteps = %+v, want only the stuck step", got)
			}
		})
	}
}

// TestListStuckSteps_OnlyStepsOfLiveJobs pins the job condition: a step that
// looks stuck by its tasks alone is listed only when its job is not terminal.
// Since H4a2 a job cancel finalizes the job's steps in its own transaction, and
// migration 00033 finalized the steps of jobs canceled by earlier releases, so
// a terminal job carries an open step only in a database that has not been
// through that repair. A completed or failed job can carry the same shape. None
// of them has downstream work that needs its steps finalized, and the terminal
// jobs are the migration's to repair, not this start-up pass's, which on a
// healthy farm must stay one query and no writes. A running job (the F6 case)
// and a paused one are live and ARE listed.
func TestListStuckSteps_OnlyStepsOfLiveJobs(t *testing.T) {
	cases := []struct {
		jobStatus store.JobStatus
		task      store.TaskStatus // the terminal task the stuck-looking step holds
		listed    bool
	}{
		{store.JobStatusRunning, store.TaskStatusSucceeded, true},
		{store.JobStatusPaused, store.TaskStatusSucceeded, true},
		{store.JobStatusCanceled, store.TaskStatusCanceled, false}, // what releases before H4a2 left behind (migration 00033 repairs it)
		{store.JobStatusCompleted, store.TaskStatusSucceeded, false},
		{store.JobStatusFailed, store.TaskStatusFailed, false},
	}
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			// One job per case, all in the first job's farm and queue.
			var shared *h4aGraph
			stepOf := map[store.JobStatus]string{}
			wantCount := 0
			for _, tc := range cases {
				g := seedGraph(t, st, graphOpts{jobStatus: tc.jobStatus, share: shared}, stepSpec{
					name: "s", status: store.StepStatusReady, tasks: []store.TaskStatus{tc.task},
				})
				if shared == nil {
					shared = &g
				}
				stepOf[tc.jobStatus] = g.Steps["s"].ID
				if tc.listed {
					wantCount++
				}
			}

			stuck, err := st.ListStuckSteps(t.Context())
			if err != nil {
				t.Fatalf("ListStuckSteps: %v", err)
			}
			got := map[string]bool{}
			for _, s := range stuck {
				got[s.ID] = true
			}
			for _, tc := range cases {
				if listed := got[stepOf[tc.jobStatus]]; listed != tc.listed {
					t.Errorf("step of a %s job: listed = %v, want %v", tc.jobStatus, listed, tc.listed)
				}
			}
			if len(stuck) != wantCount {
				t.Errorf("ListStuckSteps returned %d steps, want %d", len(stuck), wantCount)
			}
		})
	}
}
