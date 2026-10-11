// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// jobIn returns a job seed in g's farm and queue with a fresh ID.
func jobIn(g jobGraph) *storetest.JobSeed {
	return storetest.NewJob(uuid.NewString(), g.Farm.ID, g.Queue.ID)
}

func jobIDs(jobs []store.Job) []string {
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	return ids
}

func sorted(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func TestParity_ListBlockedJobs(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			up := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning})
			if got, err := st.ListBlockedJobs(t.Context()); err != nil || len(got) != 0 {
				t.Fatalf("no blocked jobs: ListBlockedJobs = (%v, %v), want none", jobIDs(got), err)
			}
			b1, b2 := blockedOn(t, st, up), blockedOn(t, st, up)
			seedGraph(t, st, graphOpts{share: &up}) // pending: not blocked

			got, err := st.ListBlockedJobs(t.Context())
			if err != nil {
				t.Fatalf("ListBlockedJobs: %v", err)
			}
			if ids, want := sorted(jobIDs(got)...), sorted(b1.Job.ID, b2.Job.ID); !slices.Equal(ids, want) {
				t.Fatalf("ListBlockedJobs = %v, want %v", ids, want)
			}
			for _, j := range got {
				if j.Status != store.JobStatusBlocked {
					t.Fatalf("listed job %s is %s, want blocked", j.ID, j.Status)
				}
			}
		})
	}
}

// seedAutoParked submits a job paused by the failure-limit sweep: a park
// reason and four genuine failures, with job parameters.
func seedAutoParked(t *testing.T, st store.Store, g jobGraph) store.Job {
	t.Helper()
	seed := jobIn(g).As(store.JobStatusPaused)
	seed.Sub.Job.ParkReason, seed.Sub.Job.FailedAttempts = "failure limit reached (4)", 4
	seed.Sub.Job.Parameters = map[string]string{"Frames": "1-10"}
	return seed.Submit(t, st).Job
}

func TestParity_UpdateJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedGraph(t, st, graphOpts{})
			job := seedAutoParked(t, st, g)

			attempts := 9
			upd := job
			upd.Name, upd.Owner, upd.Submitter, upd.Project = "renamed", "bob", "svc", "proj"
			upd.Priority, upd.MaxAttempts = 80, &attempts
			upd.Status = store.JobStatusCompleted // a lifecycle column: must be ignored
			got, err := st.UpdateJob(ctx, upd)
			if err != nil {
				t.Fatalf("UpdateJob: %v", err)
			}
			if got.Name != "renamed" || got.Owner != "bob" || got.Submitter != "svc" || got.Project != "proj" ||
				got.Priority != 80 || got.MaxAttempts == nil || *got.MaxAttempts != 9 {
				t.Fatalf("UpdateJob = %+v, want the user-settable fields written", got)
			}
			if got.Status != store.JobStatusPaused || !got.CreatedAt.Equal(job.CreatedAt) || got.UpdatedAt.Before(job.UpdatedAt) {
				t.Fatalf("UpdateJob status %s created %v updated %v, want paused, created %v kept, updated advanced",
					got.Status, got.CreatedAt, got.UpdatedAt, job.CreatedAt)
			}
			stored := mustJob(t, st, job.ID)
			if stored.Name != "renamed" || stored.Priority != 80 || stored.Status != store.JobStatusPaused ||
				stored.Parameters["Frames"] != "1-10" {
				t.Fatalf("GetJob after update = %+v, want the update stored and lifecycle kept", stored)
			}
			if _, err := st.UpdateJob(ctx, store.Job{ID: uuid.NewString(), FarmID: g.Farm.ID, QueueID: g.Queue.ID, Name: "x"}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown job: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestParity_UpdateJob_KeepsFailureCounters pins that failed_attempts and
// park_reason are lifecycle columns UpdateJob never writes, and that the job
// it returns reflects their stored values.
func TestParity_UpdateJob_KeepsFailureCounters(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if name == "fake" {
				t.Skip("divergence: fake UpdateJob writes the caller's FailedAttempts and ParkReason; SQLite keeps the stored values")
			}
			g := seedGraph(t, st, graphOpts{})
			job := seedAutoParked(t, st, g)

			upd := job
			upd.FailedAttempts, upd.ParkReason = 0, ""
			got, err := st.UpdateJob(t.Context(), upd)
			if err != nil {
				t.Fatalf("UpdateJob: %v", err)
			}
			if got.FailedAttempts != 4 || got.ParkReason != job.ParkReason {
				t.Fatalf("returned failed_attempts %d park_reason %q, want 4 / %q kept", got.FailedAttempts, got.ParkReason, job.ParkReason)
			}
			if stored := mustJob(t, st, job.ID); stored.FailedAttempts != 4 || stored.ParkReason != job.ParkReason {
				t.Fatalf("stored failed_attempts %d park_reason %q, want 4 / %q kept", stored.FailedAttempts, stored.ParkReason, job.ParkReason)
			}
		})
	}
}

func TestParity_DemoteStalledJobs(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedGraph(t, st, graphOpts{})
			running := func(tasks ...store.TaskStatus) jobGraph {
				return seedGraph(t, st, graphOpts{share: &g, jobStatus: store.JobStatusRunning},
					stepSpec{name: "a", status: store.StepStatusReady, tasks: tasks})
			}
			// stalledReady started (StartedAt set) and has only ready work left.
			stalledReady := jobIn(g).As(store.JobStatusRunning).Step("s-"+uuid.NewString(), store.StepStatusReady)
			stalledReady.Task(uuid.NewString(), stalledReady.Sub.Steps[0].ID, store.TaskStatusReady)
			stalled := stalledReady.Submit(t, st).Job
			stalledPending := running(store.TaskStatusPending, store.TaskStatusSucceeded)
			inFlight := running(store.TaskStatusRunning, store.TaskStatusReady)
			assigned := running(store.TaskStatusAssigned, store.TaskStatusPending)
			allTerminal := running(store.TaskStatusSucceeded, store.TaskStatusFailed)
			pending := seedGraph(t, st, graphOpts{share: &g},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})

			now := time.Now().UTC().Add(time.Minute)
			got, err := st.DemoteStalledJobs(ctx, now)
			if err != nil {
				t.Fatalf("DemoteStalledJobs: %v", err)
			}
			if want := sorted(stalled.ID, stalledPending.Job.ID); !slices.Equal(sorted(got...), want) {
				t.Fatalf("demoted = %v, want %v", sorted(got...), want)
			}
			demoted := mustJob(t, st, stalled.ID)
			if demoted.Status != store.JobStatusPending || !demoted.UpdatedAt.Equal(now) ||
				demoted.StartedAt == nil || !demoted.StartedAt.Equal(*stalled.StartedAt) {
				t.Fatalf("demoted job = status %s updated %v started %v, want pending, updated %v, started kept %v",
					demoted.Status, demoted.UpdatedAt, demoted.StartedAt, now, stalled.StartedAt)
			}
			for _, j := range []jobGraph{inFlight, assigned, allTerminal, pending} {
				if after := mustJob(t, st, j.Job.ID); after.Status != j.Job.Status || !after.UpdatedAt.Equal(j.Job.UpdatedAt) {
					t.Fatalf("job %s = %s (updated %v), want left %s", j.Job.ID, after.Status, after.UpdatedAt, j.Job.Status)
				}
			}
			if again, err := st.DemoteStalledJobs(ctx, now); err != nil || len(again) != 0 {
				t.Fatalf("second sweep = (%v, %v), want nothing", again, err)
			}
		})
	}
}

func TestParity_ParkJob(t *testing.T) {
	cases := []struct {
		name       string
		status     store.JobStatus
		wantParked bool
	}{
		{"pending", store.JobStatusPending, true},
		{"running", store.JobStatusRunning, true},
		{"paused", store.JobStatusPaused, true},
		{"blocked", store.JobStatusBlocked, true},
		{"completed", store.JobStatusCompleted, false},
		{"failed", store.JobStatusFailed, false},
		{"canceled", store.JobStatusCanceled, false},
	}
	for name, st := range newStores(t) {
		g := seedGraph(t, st, graphOpts{})
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				job := jobIn(g).As(tc.status).Submit(t, st).Job
				now := time.Now().UTC().Add(time.Minute)
				if err := st.ParkJob(t.Context(), job.ID, "failure limit reached (3)", now); err != nil {
					t.Fatalf("ParkJob: %v", err)
				}
				got := mustJob(t, st, job.ID)
				if !tc.wantParked {
					if got.Status != tc.status || got.ParkReason != "" || !got.UpdatedAt.Equal(job.UpdatedAt) {
						t.Fatalf("terminal job = %s %q (updated %v), want left untouched", got.Status, got.ParkReason, got.UpdatedAt)
					}
					return
				}
				if got.Status != store.JobStatusPaused || got.ParkReason != "failure limit reached (3)" || !got.UpdatedAt.Equal(now) {
					t.Fatalf("parked job = %s %q (updated %v), want paused with the reason at %v", got.Status, got.ParkReason, got.UpdatedAt, now)
				}
			})
		}
		t.Run(name+"/unknown", func(t *testing.T) {
			if err := st.ParkJob(t.Context(), uuid.NewString(), "r", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown job: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_ResumeJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedGraph(t, st, graphOpts{})
			withFailures := func(status store.JobStatus) store.Job {
				seed := jobIn(g).As(status)
				seed.Sub.Job.FailedAttempts = 3
				return seed.Submit(t, st).Job
			}
			now := time.Now().UTC().Add(time.Minute)

			// Auto-parked: reason cleared and the failure count re-armed.
			parked := withFailures(store.JobStatusRunning)
			if err := st.ParkJob(ctx, parked.ID, "failure limit reached (3)", now); err != nil {
				t.Fatalf("ParkJob: %v", err)
			}
			if err := st.ResumeJob(ctx, parked.ID, now); err != nil {
				t.Fatalf("ResumeJob auto-parked: %v", err)
			}
			if got := mustJob(t, st, parked.ID); got.Status != store.JobStatusPending || got.ParkReason != "" ||
				got.FailedAttempts != 0 || !got.UpdatedAt.Equal(now) {
				t.Fatalf("resumed auto-parked job = %+v, want pending, no reason, 0 failures, updated %v", got, now)
			}

			// Manually paused: the failure count survives.
			manual := withFailures(store.JobStatusRunning)
			if err := st.PauseJob(ctx, manual.ID, now); err != nil {
				t.Fatalf("PauseJob: %v", err)
			}
			if err := st.ResumeJob(ctx, manual.ID, now); err != nil {
				t.Fatalf("ResumeJob manual: %v", err)
			}
			if got := mustJob(t, st, manual.ID); got.Status != store.JobStatusPending || got.FailedAttempts != 3 {
				t.Fatalf("resumed manual job = %s with %d failures, want pending with 3", got.Status, got.FailedAttempts)
			}

			// Not paused: a no-op.
			live := withFailures(store.JobStatusRunning)
			if err := st.ResumeJob(ctx, live.ID, now); err != nil {
				t.Fatalf("ResumeJob running: %v", err)
			}
			if got := mustJob(t, st, live.ID); got.Status != store.JobStatusRunning || got.FailedAttempts != 3 ||
				!got.UpdatedAt.Equal(live.UpdatedAt) {
				t.Fatalf("running job after resume = %+v, want untouched", got)
			}

			if err := st.ResumeJob(ctx, uuid.NewString(), now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown job: err = %v, want ErrNotFound", err)
			}
		})
	}
}
