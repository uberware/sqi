// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestStartTaskAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusAssigned, store.TaskStatusReady, store.TaskStatusReady}})
			live, superseded, reclaimed := g.Tasks["a"][0], g.Tasks["a"][1], g.Tasks["a"][2]

			// The live attempt: the task moves assigned -> running and the session is recorded.
			a := g.Attempts[live.ID]
			if ok, err := st.StartTaskAttempt(ctx, a.ID, live.ID, "sess-1", now); err != nil || !ok {
				t.Fatalf("StartTaskAttempt(live) = (%v, %v), want (true, nil)", ok, err)
			}
			if got := mustTask(t, st, live.ID).Status; got != store.TaskStatusRunning {
				t.Fatalf("live task = %q, want running", got)
			}
			if got := mustAttempt(t, st, a.ID).SessionID; got != "sess-1" {
				t.Fatalf("session = %q, want sess-1", got)
			}
			// A redelivery is harmless and still reports started.
			if ok, err := st.StartTaskAttempt(ctx, a.ID, live.ID, "sess-1", now); err != nil || !ok {
				t.Fatalf("redelivery = (%v, %v), want (true, nil)", ok, err)
			}

			// A superseded attempt: attempt 1 closed, attempt 2 open on a new lease.
			old := storetest.FailAndRequeue(t, st, leaseReq(superseded), now.Add(-time.Minute))
			storetest.Lease(t, st, store.LeaseRequest{TaskID: superseded.ID, WorkerID: fixtureWorkerID})
			if ok, err := st.StartTaskAttempt(ctx, old.ID, superseded.ID, "sess-old", now); err != nil || ok {
				t.Fatalf("StartTaskAttempt(superseded) = (%v, %v), want (false, nil)", ok, err)
			}
			if got := mustTask(t, st, superseded.ID).Status; got != store.TaskStatusAssigned {
				t.Fatalf("superseded task = %q, want assigned (untouched)", got)
			}

			// The latest attempt, but closed and the task reclaimed to ready.
			closed := storetest.FailAndRequeue(t, st, leaseReq(reclaimed), now.Add(-time.Minute))
			if ok, err := st.StartTaskAttempt(ctx, closed.ID, reclaimed.ID, "", now); err != nil || ok {
				t.Fatalf("StartTaskAttempt(closed) = (%v, %v), want (false, nil)", ok, err)
			}
			if got := mustTask(t, st, reclaimed.ID).Status; got != store.TaskStatusReady {
				t.Fatalf("reclaimed task = %q, want ready", got)
			}

			if _, err := st.StartTaskAttempt(ctx, a.ID, "no-such-task", "", now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown task: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestRecordAttemptProgress(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC().Truncate(time.Microsecond)
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusAssigned, store.TaskStatusReady, store.TaskStatusReady}})
			live, superseded, reclaimed := g.Tasks["a"][0], g.Tasks["a"][1], g.Tasks["a"][2]
			a := g.Attempts[live.ID]
			t0 := now
			forty2, fifty := 42, 50
			record := func(attemptID, taskID string, p *int, text string, at time.Time) (store.ProgressOutcome, error) {
				return st.RecordAttemptProgress(ctx, store.AttemptProgress{AttemptID: attemptID, TaskID: taskID, Progress: p, StatusText: text, At: at})
			}
			want := func(label string, got, wantOutcome store.ProgressOutcome, err error) {
				t.Helper()
				if err != nil || got != wantOutcome {
					t.Fatalf("%s = (%v, %v), want (%v, nil)", label, got, err, wantOutcome)
				}
			}

			// Still assigned: nothing is written.
			out, err := record(a.ID, live.ID, &forty2, "early", t0)
			want("assigned", out, store.ProgressTaskAssigned, err)
			if got := mustAttempt(t, st, a.ID); got.Progress != nil || got.StatusText != "" || got.ProgressAt != nil {
				t.Fatalf("assigned attempt written: %+v", got)
			}

			if ok, err := st.StartTaskAttempt(ctx, a.ID, live.ID, "", now); err != nil || !ok {
				t.Fatalf("StartTaskAttempt = (%v, %v)", ok, err)
			}
			out, err = record(a.ID, live.ID, &forty2, "frame 3", t0)
			want("first", out, store.ProgressRecorded, err)
			got := mustAttempt(t, st, a.ID)
			if got.Progress == nil || *got.Progress != 42 || got.StatusText != "frame 3" || got.ProgressAt == nil || !got.ProgressAt.Equal(t0) {
				t.Fatalf("attempt after first record = %+v", got)
			}

			for _, at := range []time.Time{t0, t0.Add(-time.Second)} {
				out, err = record(a.ID, live.ID, &fifty, "stale", at)
				want("out of order", out, store.ProgressOutOfOrder, err)
			}
			got = mustAttempt(t, st, a.ID)
			if got.Progress == nil || *got.Progress != 42 || got.StatusText != "frame 3" || !got.ProgressAt.Equal(t0) {
				t.Fatalf("out-of-order write changed the attempt: %+v", got)
			}

			out, err = record(a.ID, live.ID, &fifty, "", t0.Add(time.Second))
			want("overwrite", out, store.ProgressRecorded, err)
			got = mustAttempt(t, st, a.ID)
			if got.Progress == nil || *got.Progress != 50 || got.StatusText != "" {
				t.Fatalf("attempt after overwrite = %+v", got)
			}
			out, err = record(a.ID, live.ID, nil, "text only", t0.Add(2*time.Second))
			want("nil progress", out, store.ProgressRecorded, err)
			if got = mustAttempt(t, st, a.ID); got.Progress != nil || got.StatusText != "text only" {
				t.Fatalf("attempt after nil progress = %+v", got)
			}

			// A superseded attempt.
			old := storetest.FailAndRequeue(t, st, leaseReq(superseded), now.Add(-time.Minute))
			next := storetest.Lease(t, st, store.LeaseRequest{TaskID: superseded.ID, WorkerID: fixtureWorkerID})
			out, err = record(old.ID, superseded.ID, &forty2, "old", t0)
			want("superseded", out, store.ProgressNotLive, err)
			if got := mustAttempt(t, st, next.ID); got.Progress != nil || got.StatusText != "" || got.ProgressAt != nil {
				t.Fatalf("new attempt after a requeue = %+v, want no progress", got)
			}

			// The latest attempt, closed, task back to ready.
			closed := storetest.FailAndRequeue(t, st, leaseReq(reclaimed), now.Add(-time.Minute))
			out, err = record(closed.ID, reclaimed.ID, &forty2, "closed", t0)
			want("closed", out, store.ProgressNotLive, err)

			if _, err := record(a.ID, "no-such-task", &forty2, "", t0); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown task: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestCompleteTaskAttempt_RefusesATaskOutOfFlight pins that a late terminal
// report from the latest attempt, after the task left flight with no new lease
// (canceled then retried), does not move it.
func TestCompleteTaskAttempt_RefusesATaskOutOfFlight(t *testing.T) {
	for _, current := range []store.TaskStatus{store.TaskStatusReady, store.TaskStatusPending} {
		for name, st := range newStores(t) {
			t.Run(string(current)+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
					stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusAssigned}})
				task := g.Tasks["a"][0]
				a := cancelThenRetry(t, st, g, current)
				res, err := st.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
					AttemptID: a.ID, TaskID: task.ID, TaskStatus: store.TaskStatusCanceled,
					AttemptStatus: store.AttemptStatusCanceled, EndedAt: time.Now().UTC(),
				})
				if err != nil || !res.Rejected || res.Applied {
					t.Fatalf("CompleteTaskAttempt = (%+v, %v), want Rejected", res, err)
				}
				if got := mustTask(t, st, task.ID).Status; got != current {
					t.Fatalf("task = %q, want %q (untouched)", got, current)
				}
			})
		}
	}
}

// cancelThenRetry cancels g's one leased task in step "a", closing its attempt
// as canceled, and retries it so it comes back in want: ready while its step is
// still ready, pending once the step was finalized first. It returns the
// canceled attempt, still the task's latest.
func cancelThenRetry(t *testing.T, st store.Store, g jobGraph, want store.TaskStatus) store.TaskAttempt {
	t.Helper()
	ctx, now := t.Context(), time.Now().UTC()
	task := g.Tasks["a"][0]
	if _, ok, err := st.CancelTaskExecution(ctx, task.ID, store.FailureReasonCanceledByUser, now); err != nil || !ok {
		t.Fatalf("CancelTaskExecution = (%v, %v), want canceled", ok, err)
	}
	if want == store.TaskStatusPending {
		if status, _, err := st.FinalizeStep(ctx, g.Steps["a"].ID, now); err != nil || status != store.StepStatusCanceled {
			t.Fatalf("FinalizeStep = (%s, %v), want canceled", status, err)
		}
	}
	revived, err := st.RetryTasks(ctx, g.Job.ID, []string{task.ID}, now)
	if err != nil || len(revived) != 1 || revived[0].Status != want {
		t.Fatalf("RetryTasks = (%+v, %v), want the task revived %s", revived, err, want)
	}
	a := mustAttempt(t, st, g.Attempts[task.ID].ID)
	if a.Status != store.AttemptStatusCanceled {
		t.Fatalf("attempt = %q, want canceled", a.Status)
	}
	return a
}

func TestRequeueTaskForRetry_GuardsOnTheLatestAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})
			task := g.Tasks["a"][0]
			failed := storetest.FailAndRequeue(t, st, leaseReq(task), now.Add(-time.Minute))
			fresh := storetest.Lease(t, st, store.LeaseRequest{TaskID: task.ID, WorkerID: fixtureWorkerID}) // a new lease
			if ok, err := st.RequeueTaskForRetry(ctx, task.ID, failed.ID, now, now); err != nil || ok {
				t.Fatalf("requeue on a superseded attempt = (%v, %v), want (false, nil)", ok, err)
			}
			if got := mustTask(t, st, task.ID).Status; got != store.TaskStatusAssigned {
				t.Fatalf("task = %q, want assigned (the new lease)", got)
			}
			if ok, err := st.RequeueTaskForRetry(ctx, task.ID, fresh.ID, now, now); err != nil || !ok {
				t.Fatalf("requeue on the latest attempt = (%v, %v), want (true, nil)", ok, err)
			}
			if ok, err := st.RequeueTaskForRetry(ctx, "no-such-task", fresh.ID, now, now); err != nil || ok {
				t.Fatalf("requeue of a missing task = (%v, %v), want (false, nil)", ok, err)
			}
		})
	}
}

func TestReclaimTaskAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady, store.TaskStatusAssigned, store.TaskStatusRunning}})
			running, canceled, userCanceled := g.Tasks["a"][0], g.Tasks["a"][1], g.Tasks["a"][2]
			pool := seedPool(t, st, 1)
			a := leaseClaiming(t, st, running.ID, true, pool)

			if ok, err := st.ReclaimTaskAttempt(ctx, a.ID, running.ID, now); err != nil || !ok {
				t.Fatalf("ReclaimTaskAttempt = (%v, %v), want (true, nil)", ok, err)
			}
			got := mustTask(t, st, running.ID)
			if got.Status != store.TaskStatusReady || got.AssignedWorkerID != "" || got.FailedAttempts != 0 {
				t.Fatalf("task = %+v, want ready, unassigned, no failure counted", got)
			}
			if at := mustAttempt(t, st, a.ID); at.Status != store.AttemptStatusFailed || at.Message != store.FailureReasonWorkerShutdown {
				t.Fatalf("attempt = %+v, want failed with the shutdown message", at)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
			if j := mustJob(t, st, g.Job.ID); j.FailedAttempts != 0 {
				t.Fatalf("job failed_attempts = %d, want 0", j.FailedAttempts)
			}
			// Again: the attempt is closed, so nothing happens.
			if ok, err := st.ReclaimTaskAttempt(ctx, a.ID, running.ID, now); err != nil || ok {
				t.Fatalf("second ReclaimTaskAttempt = (%v, %v), want (false, nil)", ok, err)
			}
			// A task the user already canceled before it started stays canceled.
			c := g.Attempts[canceled.ID]
			if _, ok, err := st.CancelTaskExecution(ctx, canceled.ID, store.FailureReasonCanceledByUser, now); err != nil || !ok {
				t.Fatalf("CancelTaskExecution(assigned) = (%v, %v), want (true, nil)", ok, err)
			}
			if ok, err := st.ReclaimTaskAttempt(ctx, c.ID, canceled.ID, now); err != nil || ok {
				t.Fatalf("ReclaimTaskAttempt(canceled) = (%v, %v), want (false, nil)", ok, err)
			}
			if got := mustTask(t, st, canceled.ID).Status; got != store.TaskStatusCanceled {
				t.Fatalf("canceled task = %q, want canceled", got)
			}
			if at := mustAttempt(t, st, c.ID); at.Status != store.AttemptStatusCanceled || at.Message == store.FailureReasonWorkerShutdown {
				t.Fatalf("canceled attempt = %+v, want canceled and untouched", at)
			}
			// The same again for a running task (the case above was assigned):
			// the user cancels it, then its worker's shutdown report arrives for
			// the attempt the cancel closed.
			u := g.Attempts[userCanceled.ID]
			if _, ok, err := st.CancelTaskExecution(ctx, userCanceled.ID, store.FailureReasonCanceledByUser, now); err != nil || !ok {
				t.Fatalf("CancelTaskExecution = (%v, %v), want (true, nil)", ok, err)
			}
			if ok, err := st.ReclaimTaskAttempt(ctx, u.ID, userCanceled.ID, now); err != nil || ok {
				t.Fatalf("ReclaimTaskAttempt(user-canceled) = (%v, %v), want (false, nil)", ok, err)
			}
			if got := mustTask(t, st, userCanceled.ID).Status; got != store.TaskStatusCanceled {
				t.Fatalf("user-canceled task = %q, want canceled", got)
			}
			if got := mustAttempt(t, st, u.ID).Status; got != store.AttemptStatusCanceled {
				t.Fatalf("user-canceled attempt = %q, want canceled", got)
			}

			if _, err := st.ReclaimTaskAttempt(ctx, a.ID, "no-such-task", now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown task: err = %v, want ErrNotFound", err)
			}
		})
	}
}
