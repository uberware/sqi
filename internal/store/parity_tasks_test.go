// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"maps"
	"math"
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

func recordProgress(t *testing.T, st store.Store, a store.TaskAttempt, progress int, text string, at time.Time) {
	t.Helper()
	out, err := st.RecordAttemptProgress(t.Context(), store.AttemptProgress{
		AttemptID: a.ID, TaskID: a.TaskID, Progress: &progress, StatusText: text, At: at,
	})
	if err != nil || out != store.ProgressRecorded {
		t.Fatalf("RecordAttemptProgress(%s) = (%v, %v), want recorded", a.TaskID, out, err)
	}
}

func TestParity_LatestAttemptProgress(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			t0 := time.Now().UTC().Truncate(time.Microsecond)
			t1 := t0.Add(time.Second)
			g := seedStepA(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusReady, store.TaskStatusReady)
			a, b, c, d, e := g.Tasks["a"][0], g.Tasks["a"][1], g.Tasks["a"][2], g.Tasks["a"][3], g.Tasks["a"][4]

			recordProgress(t, st, g.Attempts[a.ID], 42, "frame 3", t0)

			recordProgress(t, st, g.Attempts[c.ID], 100, "done", t1)
			res, err := st.CompleteTaskAttempt(ctx, store.AttemptCompletion{
				AttemptID: g.Attempts[c.ID].ID, TaskID: c.ID, TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: time.Now().UTC(),
			})
			if err != nil || !res.Applied {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v), want applied", res, err)
			}

			// D: a snapshot on an attempt that is then superseded by a new one.
			first := storetest.Running(t, st, leaseReq(d))
			recordProgress(t, st, first, 77, "old", t0)
			now := time.Now().UTC()
			if _, _, ok, err := st.RecordTaskFailure(ctx, first.ID, d.ID, nil, "", "boom", now); err != nil || !ok {
				t.Fatalf("RecordTaskFailure = (%v, %v)", ok, err)
			}
			if ok, err := st.RequeueTaskForRetry(ctx, d.ID, first.ID, now.Add(-time.Minute), now); err != nil || !ok {
				t.Fatalf("RequeueTaskForRetry = (%v, %v)", ok, err)
			}
			storetest.Lease(t, st, leaseReq(d))

			// E: a snapshot on an attempt whose task is requeued to ready and
			// not yet leased again; the closed attempt's snapshot is stale.
			eFirst := storetest.Running(t, st, leaseReq(e))
			recordProgress(t, st, eFirst, 55, "stale", t0)
			if _, _, ok, err := st.RecordTaskFailure(ctx, eFirst.ID, e.ID, nil, "", "boom", now); err != nil || !ok {
				t.Fatalf("RecordTaskFailure(E) = (%v, %v)", ok, err)
			}
			if ok, err := st.RequeueTaskForRetry(ctx, e.ID, eFirst.ID, now.Add(-time.Minute), now); err != nil || !ok {
				t.Fatalf("RequeueTaskForRetry(E) = (%v, %v)", ok, err)
			}

			unknown := uuid.NewString()
			got, err := st.LatestAttemptProgress(ctx, []string{a.ID, b.ID, c.ID, d.ID, e.ID, unknown})
			if err != nil {
				t.Fatalf("LatestAttemptProgress: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("got %d entries %+v, want exactly A and C", len(got), got)
			}
			check := func(label, id string, pct int, text string, at time.Time) {
				t.Helper()
				p, ok := got[id]
				if !ok || p.Progress == nil || *p.Progress != pct || p.StatusText != text || p.ProgressAt == nil || !p.ProgressAt.Equal(at) {
					t.Fatalf("%s = %+v (present %v), want %d %q %v", label, p, ok, pct, text, at)
				}
			}
			check("A", a.ID, 42, "frame 3", t0)
			check("C", c.ID, 100, "done", t1)

			for _, in := range [][]string{nil, {}} {
				if m, err := st.LatestAttemptProgress(ctx, in); err != nil || m == nil || len(m) != 0 {
					t.Fatalf("empty input = (%v, %v), want empty non-nil map", m, err)
				}
			}
		})
	}
}

func TestParity_RunningProgressByJob(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			at := time.Now().UTC()
			g := seedStepA(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusRunning)
			ts := g.Tasks["a"]
			recordProgress(t, st, g.Attempts[ts[0].ID], 40, "", at)
			recordProgress(t, st, g.Attempts[ts[1].ID], 25, "", at)
			// ts[2] reports nothing and counts as 0.
			recordProgress(t, st, g.Attempts[ts[3].ID], 100, "", at)
			if res, err := st.CompleteTaskAttempt(ctx, store.AttemptCompletion{
				AttemptID: g.Attempts[ts[3].ID].ID, TaskID: ts[3].ID, TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: time.Now().UTC(),
			}); err != nil || !res.Applied {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v)", res, err)
			}
			other := seedStepA(t, st, graphOpts{jobStatus: store.JobStatusRunning}, store.TaskStatusRunning)
			recordProgress(t, st, other.Attempts[other.Tasks["a"][0].ID], 90, "", at)

			got, err := st.RunningProgressByJob(ctx, g.Job.ID)
			if err != nil || math.Abs(got-0.65) > 1e-9 {
				t.Fatalf("RunningProgressByJob = (%v, %v), want 0.65", got, err)
			}
			if got, err := st.RunningProgressByJob(ctx, uuid.NewString()); err != nil || got != 0 {
				t.Fatalf("unknown job = (%v, %v), want 0", got, err)
			}
		})
	}
}

func TestParity_RunningProgressByJobs(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			at := time.Now().UTC()

			// g: two snapshots, one running task with none, one succeeded at 100.
			g := seedStepA(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusRunning, store.TaskStatusRunning)
			ts := g.Tasks["a"]
			recordProgress(t, st, g.Attempts[ts[0].ID], 40, "", at)
			recordProgress(t, st, g.Attempts[ts[1].ID], 25, "", at)
			recordProgress(t, st, g.Attempts[ts[3].ID], 100, "", at)
			if res, err := st.CompleteTaskAttempt(ctx, store.AttemptCompletion{
				AttemptID: g.Attempts[ts[3].ID].ID, TaskID: ts[3].ID, TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: time.Now().UTC(),
			}); err != nil || !res.Applied {
				t.Fatalf("CompleteTaskAttempt = (%+v, %v)", res, err)
			}
			// h: a single running task.
			h := seedStepA(t, st, graphOpts{jobStatus: store.JobStatusRunning}, store.TaskStatusRunning)
			recordProgress(t, st, h.Attempts[h.Tasks["a"][0].ID], 90, "", at)
			// idle: nothing running.
			idle := seedStepA(t, st, graphOpts{jobStatus: store.JobStatusRunning}, store.TaskStatusReady)
			// outside: not asked for, must not leak.
			outside := seedStepA(t, st, graphOpts{jobStatus: store.JobStatusRunning}, store.TaskStatusRunning)
			recordProgress(t, st, outside.Attempts[outside.Tasks["a"][0].ID], 77, "", at)

			unknown := uuid.NewString()
			ids := []string{g.Job.ID, h.Job.ID, idle.Job.ID, unknown}
			got, err := st.RunningProgressByJobs(ctx, ids)
			if err != nil {
				t.Fatalf("RunningProgressByJobs: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("got %v, want exactly the two jobs with running tasks", got)
			}
			if _, ok := got[idle.Job.ID]; ok {
				t.Fatalf("job without running tasks present: %v", got)
			}
			if _, ok := got[unknown]; ok {
				t.Fatalf("unknown job present: %v", got)
			}
			if _, ok := got[outside.Job.ID]; ok {
				t.Fatalf("unrequested job leaked: %v", got)
			}
			if math.Abs(got[g.Job.ID]-0.65) > 1e-9 || math.Abs(got[h.Job.ID]-0.9) > 1e-9 {
				t.Fatalf("got %v, want g=0.65 h=0.9", got)
			}
			for _, id := range ids {
				single, err := st.RunningProgressByJob(ctx, id)
				if err != nil || single != got[id] {
					t.Fatalf("job %s: single = (%v, %v), batch = %v", id, single, err, got[id])
				}
			}

			for _, in := range [][]string{nil, {}} {
				if m, err := st.RunningProgressByJobs(ctx, in); err != nil || m == nil || len(m) != 0 {
					t.Fatalf("empty input = (%v, %v), want empty non-nil map", m, err)
				}
			}
		})
	}
}
