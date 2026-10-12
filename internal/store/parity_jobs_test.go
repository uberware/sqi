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

func TestParity_DeleteJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			// A running upstream job, for the outgoing edge.
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning})
			upstream := g.Job

			// A pool for the usage claim.
			pool := seedPool(t, st, 10)

			// The job to delete, pending so its task can be leased.
			delWithDeps := jobIn(g)
			delWithDeps.As(store.JobStatusPending)
			delWithDeps.Sub.DependsOn = []string{upstream.ID}
			delWithDeps.Step(uuid.NewString(), store.StepStatusReady)
			delWithDeps.Task(uuid.NewString(), delWithDeps.Sub.Steps[0].ID, store.TaskStatusReady)
			delSub := delWithDeps.Submit(t, st)
			toDelete := delSub.Job
			toDeleteStepID := delSub.Steps[0].ID
			toDeleteTaskID := delSub.Tasks[0].ID

			// Leasing the task with a claim on the pool creates an attempt and a claim.
			attempt := leaseClaiming(t, st, toDeleteTaskID, false, pool)
			attemptID := attempt.ID

			// A task log for the attempt.
			if _, err := st.CreateTaskLog(ctx, mkTaskLog(attempt, 1, "test log\n")); err != nil {
				t.Fatalf("CreateTaskLog: %v", err)
			}

			// A downstream job that depends on toDelete.
			downstream := jobIn(g).As(store.JobStatusPending)
			downstream.Sub.DependsOn = []string{toDelete.ID}
			downstreamJob := downstream.Submit(t, st).Job

			// Before the delete, the attempt, the claim and the log exist.
			beforeAttempts, err := st.ListTaskAttempts(ctx, toDeleteTaskID)
			if err != nil {
				t.Fatalf("ListTaskAttempts before delete: %v", err)
			}
			if len(beforeAttempts) != 1 || beforeAttempts[0].ID != attemptID {
				t.Fatalf("before delete: expected 1 attempt, got %d", len(beforeAttempts))
			}

			beforeClaimCount, err := st.ActiveClaimCount(ctx, pool.ID)
			if err != nil {
				t.Fatalf("ActiveClaimCount before delete: %v", err)
			}
			if beforeClaimCount != 1 {
				t.Fatalf("before delete: expected 1 active claim, got %d", beforeClaimCount)
			}

			beforeLogs, err := st.ListTaskLogs(ctx, attemptID, 0, 100)
			if err != nil {
				t.Fatalf("ListTaskLogs before delete: %v", err)
			}
			if len(beforeLogs) != 1 {
				t.Fatalf("before delete: expected 1 log, got %d", len(beforeLogs))
			}

			// Both edges exist before the delete.
			beforeDeps, err := st.ListDependents(ctx, upstream.ID)
			if err != nil {
				t.Fatalf("ListDependents: %v", err)
			}
			if !slices.Contains(beforeDeps, toDelete.ID) {
				t.Fatalf("toDelete should be a dependent of upstream before delete")
			}
			downstreamDeps, err := st.ListJobDependencyIDs(ctx, downstreamJob.ID)
			if err != nil {
				t.Fatalf("ListJobDependencyIDs: %v", err)
			}
			if !slices.Contains(downstreamDeps, toDelete.ID) {
				t.Fatalf("downstreamJob should depend on toDelete before delete")
			}

			if err := st.DeleteJob(ctx, toDelete.ID); err != nil {
				t.Fatalf("DeleteJob: %v", err)
			}

			// After the delete, the job, its steps and its tasks are gone.
			if _, err := st.GetJob(ctx, toDelete.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetJob after delete: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetStep(ctx, toDeleteStepID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetStep after delete: err = %v, want ErrNotFound", err)
			}
			steps, err := st.ListSteps(ctx, toDelete.ID)
			if err != nil {
				t.Fatalf("ListSteps: %v", err)
			}
			if len(steps) != 0 {
				t.Fatalf("ListSteps after delete: got %d, want 0", len(steps))
			}
			if _, err := st.GetTask(ctx, toDeleteTaskID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetTask after delete: err = %v, want ErrNotFound", err)
			}

			// Cascade: attempts are deleted.
			afterAttempts, err := st.ListTaskAttempts(ctx, toDeleteTaskID)
			if err != nil {
				t.Fatalf("ListTaskAttempts after delete: %v", err)
			}
			if len(afterAttempts) != 0 {
				t.Fatalf("ListTaskAttempts after delete: got %d, want 0", len(afterAttempts))
			}

			// Cascade: the claim is no longer active.
			afterClaimCount, err := st.ActiveClaimCount(ctx, pool.ID)
			if err != nil {
				t.Fatalf("ActiveClaimCount after delete: %v", err)
			}
			if afterClaimCount != 0 {
				t.Fatalf("after delete: expected 0 active claims, got %d", afterClaimCount)
			}

			// Cascade: logs are deleted.
			afterLogs, err := st.ListTaskLogs(ctx, attemptID, 0, 100)
			if err != nil {
				t.Fatalf("ListTaskLogs after delete: %v", err)
			}
			if len(afterLogs) != 0 {
				t.Fatalf("ListTaskLogs after delete: got %d, want 0", len(afterLogs))
			}

			// Outgoing edges are deleted.
			afterDeps, err := st.ListDependents(ctx, upstream.ID)
			if err != nil {
				t.Fatalf("ListDependents: %v", err)
			}
			if slices.Contains(afterDeps, toDelete.ID) {
				t.Fatalf("toDelete should NOT be a dependent of upstream after delete (outgoing edge deleted)")
			}

			// Incoming edges are kept.
			downstreamDepsAfter, err := st.ListJobDependencyIDs(ctx, downstreamJob.ID)
			if err != nil {
				t.Fatalf("ListJobDependencyIDs: %v", err)
			}
			if !slices.Contains(downstreamDepsAfter, toDelete.ID) {
				t.Fatalf("downstreamJob should still depend on (deleted) toDelete after delete (incoming edge kept)")
			}

			// Second delete returns ErrNotFound.
			if err := st.DeleteJob(ctx, toDelete.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete: err = %v, want ErrNotFound", err)
			}

			// Other jobs are unaffected.
			if _, err := st.GetJob(ctx, upstream.ID); err != nil {
				t.Fatalf("upstream job: %v, want it kept", err)
			}
			if _, err := st.GetJob(ctx, downstreamJob.ID); err != nil {
				t.Fatalf("downstream job: %v, want it kept", err)
			}
		})
	}
}

func TestParity_ListDependents(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning})
			upstream := g.Job

			// No dependents yet.
			if got, err := st.ListDependents(ctx, upstream.ID); err != nil || len(got) != 0 {
				t.Fatalf("no dependents: ListDependents = (%v, %v), want none", got, err)
			}

			// A dependent and an unrelated job.
			dependent := jobIn(g).As(store.JobStatusPending)
			dependent.Sub.DependsOn = []string{upstream.ID}
			depJob := dependent.Submit(t, st).Job

			unrelated := jobIn(g).As(store.JobStatusPending).Submit(t, st).Job

			// The dependent is listed; the unrelated job is not.
			got, err := st.ListDependents(ctx, upstream.ID)
			if err != nil {
				t.Fatalf("ListDependents: %v", err)
			}
			if want := []string{depJob.ID}; !slices.Equal(sorted(got...), sorted(want...)) {
				t.Fatalf("ListDependents = %v, want %v", sorted(got...), sorted(want...))
			}

			// Unrelated job has no dependents.
			if got, err := st.ListDependents(ctx, unrelated.ID); err != nil || len(got) != 0 {
				t.Fatalf("unrelated has no dependents: ListDependents = (%v, %v), want none", got, err)
			}

			// An unknown upstream ID has no dependents.
			if got, err := st.ListDependents(ctx, uuid.NewString()); err != nil || len(got) != 0 {
				t.Fatalf("unknown upstream: ListDependents = (%v, %v), want none", got, err)
			}
		})
	}
}

// TestParity_SearchBackslashTerm pins that a search term holding a backslash,
// a percent sign or an underscore matches the same jobs on every backend. SQLite's LIKE has
// no escape character; PostgreSQL's ILIKE treats backslash as one unless told
// an empty ESCAPE clause, which would make a Windows path match nothing.
func TestParity_SearchBackslashTerm(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm, queue := seedFarmQueue(t, st)
			seedJobRow(t, st, "j-path", farm.ID, queue.ID, `render C:\shows\ep01`)
			seedJobRow(t, st, "j-pct", farm.ID, queue.ID, "100% done_now")
			seedJobRow(t, st, "j-other", farm.ID, queue.ID, "other")
			for term, want := range map[string][]string{
				`C:\shows`: {"j-path"},
				`100%`:     {"j-pct"},
				`done_now`: {"j-pct"},
			} {
				page, err := st.ListJobs(t.Context(), store.ListJobsOptions{Search: term, Pagination: store.Pagination{Limit: 10}})
				if err != nil {
					t.Fatalf("ListJobs(%q): %v", term, err)
				}
				var got []string
				for _, j := range page.Items {
					got = append(got, j.ID)
				}
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Errorf("Search %q = %v, want %v", term, got, want)
				}
			}
		})
	}
}
