// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestParity_CountActiveTasksInFarm(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusAssigned, store.TaskStatusRunning,
				store.TaskStatusReady, store.TaskStatusSucceeded, store.TaskStatusFailed)
			// A second queue in the same farm counts too.
			q2 := mustCreateQueue(t, st, store.Queue{FarmID: g.Farm.ID, Name: "second"})
			other := storetest.NewJob(uuid.NewString(), g.Farm.ID, q2.ID).Step("s-"+uuid.NewString(), store.StepStatusReady)
			taskID := uuid.NewString()
			other.Task(taskID, other.Sub.Steps[0].ID, store.TaskStatusReady).Submit(t, st)
			storetest.Lease(t, st, store.LeaseRequest{TaskID: taskID, WorkerID: fixtureWorkerID})
			// Another farm does not.
			seedStepA(t, st, graphOpts{}, store.TaskStatusRunning)

			if n, err := st.CountActiveTasksInFarm(ctx, g.Farm.ID); err != nil || n != 3 {
				t.Fatalf("CountActiveTasksInFarm = (%d, %v), want 3", n, err)
			}
			if n, err := st.CountActiveTasksInFarm(ctx, uuid.NewString()); err != nil || n != 0 {
				t.Fatalf("unknown farm = (%d, %v), want 0", n, err)
			}
		})
	}
}

func TestParity_CountReadyTasksByQueue(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now().UTC()
			// q1: two plain ready tasks, one whose backoff has elapsed, one still
			// backing off, and in-flight work.
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady, store.TaskStatusReady,
				store.TaskStatusReady, store.TaskStatusReady, store.TaskStatusRunning, store.TaskStatusPending)
			storetest.FailAndRequeue(t, st, leaseReq(g.Tasks["a"][2]), now.Add(-time.Minute))
			storetest.FailAndRequeue(t, st, leaseReq(g.Tasks["a"][3]), now.Add(time.Hour))
			// A paused job in q1: its ready task is not leasable.
			seedGraph(t, st, graphOpts{share: &g, jobStatus: store.JobStatusPaused},
				stepSpec{name: "p", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}})
			// A paused queue in the same farm: omitted.
			paused := mustCreateQueue(t, st, store.Queue{FarmID: g.Farm.ID, Name: "paused", Paused: true})
			pj := storetest.NewJob(uuid.NewString(), g.Farm.ID, paused.ID).Step("s-"+uuid.NewString(), store.StepStatusReady)
			pj.Task(uuid.NewString(), pj.Sub.Steps[0].ID, store.TaskStatusReady).Submit(t, st)
			// A queue with no ready work: omitted.
			mustCreateQueue(t, st, store.Queue{FarmID: g.Farm.ID, Name: "idle"})
			// Another farm: not counted.
			other := seedStepA(t, st, graphOpts{}, store.TaskStatusReady)

			got, err := st.CountReadyTasksByQueue(ctx, g.Farm.ID, now)
			if err != nil {
				t.Fatalf("CountReadyTasksByQueue: %v", err)
			}
			if want := map[string]int{g.Queue.ID: 3}; !maps.Equal(got, want) {
				t.Fatalf("CountReadyTasksByQueue = %v, want %v", got, want)
			}
			if got, err := st.CountReadyTasksByQueue(ctx, other.Farm.ID, now); err != nil || !maps.Equal(got, map[string]int{other.Queue.ID: 1}) {
				t.Fatalf("other farm = (%v, %v), want {%s: 1}", got, err, other.Queue.ID)
			}
		})
	}
}

func TestParity_CountTasksByJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady, store.TaskStatusReady,
				store.TaskStatusRunning, store.TaskStatusSucceeded)
			seedStepA(t, st, graphOpts{share: &g}, store.TaskStatusFailed) // another job

			got, err := st.CountTasksByJob(ctx, g.Job.ID)
			if err != nil {
				t.Fatalf("CountTasksByJob: %v", err)
			}
			want := map[store.TaskStatus]int{store.TaskStatusReady: 2, store.TaskStatusRunning: 1, store.TaskStatusSucceeded: 1}
			if !maps.Equal(got, want) {
				t.Fatalf("CountTasksByJob = %v, want %v (zero statuses omitted)", got, want)
			}
			if got, err := st.CountTasksByJob(ctx, uuid.NewString()); err != nil || len(got) != 0 {
				t.Fatalf("unknown job = (%v, %v), want empty", got, err)
			}
		})
	}
}

func TestParity_CommittedCores(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedGraph(t, st, graphOpts{})
			two, one, four := 2, 1, 4
			seed := jobIn(g).Step("s-"+uuid.NewString(), store.StepStatusReady)
			step := seed.Sub.Steps[0].ID
			ids := map[string]string{}
			for _, k := range []string{"assigned2", "runningNil", "otherWorker1", "done4", "ready4"} {
				ids[k] = uuid.NewString()
			}
			seed.TaskRow(store.Task{ID: ids["assigned2"], StepID: step, Status: store.TaskStatusReady, RequiredCores: &two})
			seed.TaskRow(store.Task{ID: ids["runningNil"], StepID: step, Status: store.TaskStatusReady})
			seed.TaskRow(store.Task{ID: ids["otherWorker1"], StepID: step, Status: store.TaskStatusReady, RequiredCores: &one})
			seed.TaskRow(store.Task{ID: ids["done4"], StepID: step, Status: store.TaskStatusReady, RequiredCores: &four})
			seed.TaskRow(store.Task{ID: ids["ready4"], StepID: step, Status: store.TaskStatusReady, RequiredCores: &four})
			seed.Submit(t, st)

			storetest.Lease(t, st, store.LeaseRequest{TaskID: ids["assigned2"], WorkerID: fixtureWorkerID})
			storetest.Running(t, st, store.LeaseRequest{TaskID: ids["runningNil"], WorkerID: fixtureWorkerID})
			storetest.Lease(t, st, store.LeaseRequest{TaskID: ids["otherWorker1"], WorkerID: "w2"})
			done := storetest.Running(t, st, store.LeaseRequest{TaskID: ids["done4"], WorkerID: fixtureWorkerID})
			storetest.Complete(t, st, done, store.TaskStatusSucceeded)

			// 2 declared + 8 for the undeclared task; the finished and ready tasks hold nothing.
			if n, err := st.CommittedCores(ctx, fixtureWorkerID, 8); err != nil || n != 10 {
				t.Fatalf("CommittedCores(w1, 8) = (%d, %v), want 10", n, err)
			}
			if n, err := st.CommittedCores(ctx, "w2", 8); err != nil || n != 1 {
				t.Fatalf("CommittedCores(w2, 8) = (%d, %v), want 1", n, err)
			}
			if n, err := st.CommittedCores(ctx, "idle", 8); err != nil || n != 0 {
				t.Fatalf("CommittedCores(idle) = (%d, %v), want 0", n, err)
			}
		})
	}
}

func TestParity_CountUnschedulableTasksByJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady, store.TaskStatusReady, store.TaskStatusReady)
			other := seedStepA(t, st, graphOpts{share: &g}, store.TaskStatusReady)
			for _, task := range []store.Task{g.Tasks["a"][0], g.Tasks["a"][1], other.Tasks["a"][0]} {
				if ok, err := st.SetTaskUnschedulableReason(ctx, task.ID, "no worker has gpu"); err != nil || !ok {
					t.Fatalf("SetTaskUnschedulableReason = (%v, %v), want written", ok, err)
				}
			}
			if n, err := st.CountUnschedulableTasksByJob(ctx, g.Job.ID); err != nil || n != 2 {
				t.Fatalf("CountUnschedulableTasksByJob = (%d, %v), want 2", n, err)
			}
			// A task that leaves ready no longer counts: a lease clears it.
			storetest.Lease(t, st, leaseReq(g.Tasks["a"][0]))
			if _, err := st.SetTaskUnschedulableReason(ctx, g.Tasks["a"][1].ID, ""); err != nil {
				t.Fatalf("clear reason: %v", err)
			}
			if n, err := st.CountUnschedulableTasksByJob(ctx, g.Job.ID); err != nil || n != 0 {
				t.Fatalf("after lease and clear = (%d, %v), want 0", n, err)
			}
			if n, err := st.CountUnschedulableTasksByJob(ctx, uuid.NewString()); err != nil || n != 0 {
				t.Fatalf("unknown job = (%d, %v), want 0", n, err)
			}
		})
	}
}

func TestParity_FailureReasonSummary(t *testing.T) {
	failed := store.TaskStatusFailed
	cases := []struct {
		name    string
		tasks   []store.TaskStatus
		reasons []string
		want    store.FailureSummary
	}{
		{
			"dominant by count",
			[]store.TaskStatus{failed, failed, failed, failed, store.TaskStatusCanceled, store.TaskStatusSucceeded},
			[]string{"oom", "exit 1", "exit 1", "", "canceled by user", ""},
			store.FailureSummary{FailedCount: 3, DominantReason: "exit 1", DistinctReasons: 2},
		},
		{
			"tie broken by reason ascending",
			[]store.TaskStatus{failed, failed},
			[]string{"zebra", "apple"},
			store.FailureSummary{FailedCount: 2, DominantReason: "apple", DistinctReasons: 2},
		},
		{
			"no failed task with a reason",
			[]store.TaskStatus{failed, store.TaskStatusCanceled, store.TaskStatusReady},
			[]string{"", "canceled by user", ""},
			store.FailureSummary{},
		},
	}
	for name, st := range newStores(t) {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{},
					stepSpec{name: "a", status: store.StepStatusReady, tasks: tc.tasks, reasons: tc.reasons})
				got, err := st.FailureReasonSummary(t.Context(), g.Job.ID)
				if err != nil || got != tc.want {
					t.Fatalf("FailureReasonSummary = (%+v, %v), want %+v", got, err, tc.want)
				}
			})
		}
		t.Run(name+"/unknown job", func(t *testing.T) {
			if got, err := st.FailureReasonSummary(t.Context(), uuid.NewString()); err != nil || got != (store.FailureSummary{}) {
				t.Fatalf("unknown job = (%+v, %v), want zero", got, err)
			}
		})
	}
}

func TestParity_LatestTaskAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady, store.TaskStatusReady)
			task := g.Tasks["a"][0]
			if _, err := st.LatestTaskAttempt(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("no attempts: err = %v, want ErrNotFound", err)
			}
			first := storetest.FailAndRequeue(t, st, leaseReq(task), time.Now().UTC().Add(-time.Minute))
			if got, err := st.LatestTaskAttempt(ctx, task.ID); err != nil || got.ID != first.ID || got.AttemptNumber != 1 {
				t.Fatalf("after one attempt = (%+v, %v), want %s #1", got, err, first.ID)
			}
			second := storetest.Lease(t, st, leaseReq(task))
			got, err := st.LatestTaskAttempt(ctx, task.ID)
			if err != nil || got.ID != second.ID || got.AttemptNumber != 2 || got.Status != store.AttemptStatusRunning ||
				got.TaskID != task.ID || got.WorkerID != fixtureWorkerID {
				t.Fatalf("after two attempts = (%+v, %v), want %s #2 running", got, err, second.ID)
			}
			// Another task's attempts do not leak in.
			if _, err := st.LatestTaskAttempt(ctx, g.Tasks["a"][1].ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("sibling task: err = %v, want ErrNotFound", err)
			}
		})
	}
}
