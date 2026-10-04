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
// it. The attempt close and claim release still happen.
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
