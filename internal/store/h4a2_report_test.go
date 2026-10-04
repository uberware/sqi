// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestStartTaskAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusAssigned, store.TaskStatusAssigned, store.TaskStatusReady}})
			live, superseded, reclaimed := g.Tasks["a"][0], g.Tasks["a"][1], g.Tasks["a"][2]

			// The live attempt: the task moves assigned -> running and the session is recorded.
			a := seedAttempt(t, st, live, store.AttemptStatusRunning)
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
			old := seedAttempt(t, st, superseded, store.AttemptStatusFailed)
			seedAttempt(t, st, superseded, store.AttemptStatusRunning)
			if ok, err := st.StartTaskAttempt(ctx, old.ID, superseded.ID, "sess-old", now); err != nil || ok {
				t.Fatalf("StartTaskAttempt(superseded) = (%v, %v), want (false, nil)", ok, err)
			}
			if got := mustTask(t, st, superseded.ID).Status; got != store.TaskStatusAssigned {
				t.Fatalf("superseded task = %q, want assigned (untouched)", got)
			}

			// The latest attempt, but closed and the task reclaimed to ready.
			closed := seedAttempt(t, st, reclaimed, store.AttemptStatusFailed)
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

// TestCompleteTaskAttempt_RefusesATaskOutOfFlight pins item 9's second
// late-report hole: a late terminal report from the latest attempt, after the
// task left flight with no new lease (canceled then retried), must not move
// it.
func TestCompleteTaskAttempt_RefusesATaskOutOfFlight(t *testing.T) {
	for _, current := range []store.TaskStatus{store.TaskStatusReady, store.TaskStatusPending} {
		for name, st := range newStores(t) {
			t.Run(string(current)+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
					stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{current}})
				task := g.Tasks["a"][0]
				a := seedAttempt(t, st, task, store.AttemptStatusCanceled)
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

func TestRequeueTaskForRetry_GuardsOnTheLatestAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusAssigned}})
			task := g.Tasks["a"][0]
			failed := seedAttempt(t, st, task, store.AttemptStatusFailed)
			fresh := seedAttempt(t, st, task, store.AttemptStatusRunning) // a new lease
			if ok, err := st.RequeueTaskForRetry(ctx, task.ID, failed.ID, now, now); err != nil || ok {
				t.Fatalf("requeue on a superseded attempt = (%v, %v), want (false, nil)", ok, err)
			}
			if got := mustTask(t, st, task.ID).Status; got != store.TaskStatusAssigned {
				t.Fatalf("task = %q, want assigned (the new lease)", got)
			}
			if ok, err := st.RequeueTaskForRetry(ctx, task.ID, fresh.ID, now, now); err != nil || !ok {
				t.Fatalf("requeue on the latest attempt = (%v, %v), want (true, nil)", ok, err)
			}
		})
	}
}

func TestReclaimTaskAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning, store.TaskStatusCanceled, store.TaskStatusRunning}})
			running, canceled, userCanceled := g.Tasks["a"][0], g.Tasks["a"][1], g.Tasks["a"][2]
			pool := seedPool(t, st, 1)
			a := seedAttempt(t, st, running, store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, a.ID)

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
			// Review Focus 3: a task the user already canceled stays canceled.
			c := seedAttempt(t, st, canceled, store.AttemptStatusCanceled)
			if ok, err := st.ReclaimTaskAttempt(ctx, c.ID, canceled.ID, now); err != nil || ok {
				t.Fatalf("ReclaimTaskAttempt(canceled) = (%v, %v), want (false, nil)", ok, err)
			}
			if got := mustTask(t, st, canceled.ID).Status; got != store.TaskStatusCanceled {
				t.Fatalf("canceled task = %q, want canceled", got)
			}
			if at := mustAttempt(t, st, c.ID); at.Status != store.AttemptStatusCanceled || at.Message == store.FailureReasonWorkerShutdown {
				t.Fatalf("canceled attempt = %+v, want canceled and untouched", at)
			}
			// The same through the real cancel path: the user cancels a running
			// task, then its worker's shutdown report arrives for the attempt the
			// cancel closed.
			u := seedAttempt(t, st, userCanceled, store.AttemptStatusRunning)
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
