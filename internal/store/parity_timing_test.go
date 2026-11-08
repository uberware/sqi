// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// ranFor runs taskID, which must be ready, from start for d and ends it
// succeeded: one succeeded attempt with known started_at and ended_at.
func ranFor(t *testing.T, st store.Store, taskID string, start time.Time, d time.Duration) {
	t.Helper()
	a := storetest.Running(t, st, store.LeaseRequest{TaskID: taskID, WorkerID: fixtureWorkerID, Now: start})
	res, err := st.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
		AttemptID: a.ID, TaskID: taskID, TaskStatus: store.TaskStatusSucceeded,
		AttemptStatus: store.AttemptStatusSucceeded, EndedAt: start.Add(d),
	})
	if err != nil || !res.Applied {
		t.Fatalf("CompleteTaskAttempt(%s) = (%+v, %v), want applied", taskID, res, err)
	}
}

// timingScope creates a farm and a queue for the timing tests.
func timingScope(t *testing.T, st store.Store) (farmID, queueID string) {
	t.Helper()
	var g jobGraph
	seedScope(t, st, &g, graphOpts{})
	return g.Farm.ID, g.Queue.ID
}

func checkStepTiming(t *testing.T, got, want store.StepTiming) {
	t.Helper()
	if math.Abs(got.MeasuredSeconds-want.MeasuredSeconds) > 0.001 {
		t.Errorf("step %s MeasuredSeconds = %v, want %v", want.StepID, got.MeasuredSeconds, want.MeasuredSeconds)
	}
	got.MeasuredSeconds, want.MeasuredSeconds = 0, 0
	if got != want {
		t.Errorf("step timing = %+v, want %+v", got, want)
	}
}

func checkRunning(t *testing.T, got []store.RunningTiming, stepID string, startedAt time.Time, progress *int) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("Running = %+v, want one entry", got)
	}
	r := got[0]
	if r.StepID != stepID {
		t.Errorf("Running StepID = %s, want %s", r.StepID, stepID)
	}
	if d := r.StartedAt.Sub(startedAt); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("Running StartedAt = %v, want %v", r.StartedAt, startedAt)
	}
	if (r.Progress == nil) != (progress == nil) || (progress != nil && *r.Progress != *progress) {
		t.Errorf("Running Progress = %v, want %v", r.Progress, progress)
	}
}

func TestParity_JobTimings(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farmID, queueID := timingScope(t, st)
			t0 := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)

			// J: S1 has every task status, S2 two pending tasks.
			s1, s2 := "s1-"+uuid.NewString(), "s2-"+uuid.NewString()
			est := 5400
			j := storetest.NewJob(uuid.NewString(), farmID, queueID).As(store.JobStatusRunning).
				Step(s1, store.StepStatusReady).
				Task("succ-1", s1, store.TaskStatusReady).
				Task("succ-2", s1, store.TaskStatusReady).
				Task("fail", s1, store.TaskStatusFailed).
				Task("canc", s1, store.TaskStatusCanceled).
				Task("rdy", s1, store.TaskStatusReady).
				Task("asg", s1, store.TaskStatusAssigned).
				Task("run", s1, store.TaskStatusReady).
				Task("pend-1", s2, store.TaskStatusPending).
				Task("pend-2", s2, store.TaskStatusPending)
			j.Sub.Job.EstimatedSeconds = &est
			storetest.SubmitLeasing(t, st, j.Sub, func(store.Task) store.LeaseRequest {
				return store.LeaseRequest{WorkerID: fixtureWorkerID, Now: t0.Add(-time.Hour)}
			})
			ranFor(t, st, "succ-1", t0.Add(-20*time.Minute), 60*time.Second)
			ranFor(t, st, "succ-2", t0.Add(-10*time.Minute), 90*time.Second)
			run := storetest.Running(t, st, store.LeaseRequest{TaskID: "run", WorkerID: fixtureWorkerID, Now: t0})
			recordProgress(t, st, run, 40, "", t0.Add(time.Second))

			// K: a succeeded task whose attempt never recorded an end.
			sk := "sk-" + uuid.NewString()
			k := storetest.NewJob(uuid.NewString(), farmID, queueID).As(store.JobStatusRunning).
				Step(sk, store.StepStatusReady).
				Task("k-succ", sk, store.TaskStatusSucceeded).
				Submit(t, st)
			storetest.InjectAttempt(t, st, store.TaskAttempt{
				TaskID: "k-succ", WorkerID: fixtureWorkerID, AttemptNumber: 1,
				Status: store.AttemptStatusSucceeded, StartedAt: t0,
			})

			// N: one succeeded task whose attempt ended before it started (clock
			// skew) and one measured normally; the first must not be measured, so
			// it cannot pull the step's mean down or negative.
			sn := "sn-" + uuid.NewString()
			n := storetest.NewJob(uuid.NewString(), farmID, queueID).As(store.JobStatusRunning).
				Step(sn, store.StepStatusReady).
				Task("n-skew", sn, store.TaskStatusSucceeded).
				Task("n-ok", sn, store.TaskStatusSucceeded).
				Task("n-skew-done", sn, store.TaskStatusReady).
				Submit(t, st)
			// The same skew reached through a real completion: the worker's
			// clock puts the end 30 s before the server-stamped start.
			ranFor(t, st, "n-skew-done", t0, -30*time.Second)
			skewEnd, okEnd := t0.Add(-90*time.Second), t0.Add(60*time.Second)
			storetest.InjectAttempt(t, st, store.TaskAttempt{
				TaskID: "n-skew", WorkerID: fixtureWorkerID, AttemptNumber: 1,
				Status: store.AttemptStatusSucceeded, StartedAt: t0, EndedAt: &skewEnd,
			})
			storetest.InjectAttempt(t, st, store.TaskAttempt{
				TaskID: "n-ok", WorkerID: fixtureWorkerID, AttemptNumber: 1,
				Status: store.AttemptStatusSucceeded, StartedAt: t0, EndedAt: &okEnd,
			})

			// R: a running task on its second attempt.
			sr := "sr-" + uuid.NewString()
			r := storetest.NewJob(uuid.NewString(), farmID, queueID).As(store.JobStatusRunning).
				Step(sr, store.StepStatusReady).
				Task("r-run", sr, store.TaskStatusReady).
				Submit(t, st)
			t1 := t0.Add(-30 * time.Second)
			storetest.FailAndRequeue(t, st,
				store.LeaseRequest{TaskID: "r-run", WorkerID: fixtureWorkerID, Now: t1.Add(-time.Hour)},
				t1.Add(-time.Minute))
			storetest.Running(t, st, store.LeaseRequest{TaskID: "r-run", WorkerID: fixtureWorkerID, Now: t1})

			unknown := uuid.NewString()
			got, err := st.JobTimings(ctx, []string{j.Sub.Job.ID, k.Job.ID, n.Job.ID, r.Job.ID, unknown})
			if err != nil {
				t.Fatalf("JobTimings: %v", err)
			}
			if len(got) != 4 {
				t.Fatalf("got %d jobs %+v, want J, K, N and R", len(got), got)
			}
			if _, ok := got[unknown]; ok {
				t.Fatalf("unknown job present")
			}

			t.Run("every status", func(t *testing.T) {
				jt := got[j.Sub.Job.ID]
				if jt.JobID != j.Sub.Job.ID || jt.QueueID != queueID || jt.Status != store.JobStatusRunning {
					t.Errorf("job = %s/%s/%s, want %s/%s/running", jt.JobID, jt.QueueID, jt.Status, j.Sub.Job.ID, queueID)
				}
				if jt.EstimatedSeconds == nil || *jt.EstimatedSeconds != est {
					t.Errorf("EstimatedSeconds = %v, want %d", jt.EstimatedSeconds, est)
				}
				if len(jt.Steps) != 2 {
					t.Fatalf("Steps = %+v, want S1 and S2", jt.Steps)
				}
				checkStepTiming(t, jt.Steps[0], store.StepTiming{
					StepID: s1, NotStarted: 2, Running: 1, Succeeded: 2, Failed: 1, Canceled: 1,
					Measured: 2, MeasuredSeconds: 150,
				})
				checkStepTiming(t, jt.Steps[1], store.StepTiming{StepID: s2, NotStarted: 2})
				forty := 40
				checkRunning(t, jt.Running, s1, t0, &forty)
			})

			t.Run("succeeded without end", func(t *testing.T) {
				kt := got[k.Job.ID]
				if kt.EstimatedSeconds != nil {
					t.Errorf("EstimatedSeconds = %v, want nil", *kt.EstimatedSeconds)
				}
				if len(kt.Steps) != 1 {
					t.Fatalf("Steps = %+v, want one", kt.Steps)
				}
				checkStepTiming(t, kt.Steps[0], store.StepTiming{StepID: sk, Succeeded: 1})
				if len(kt.Running) != 0 {
					t.Errorf("Running = %+v, want none", kt.Running)
				}
			})

			t.Run("succeeded ending before it started", func(t *testing.T) {
				nt := got[n.Job.ID]
				if len(nt.Steps) != 1 {
					t.Fatalf("Steps = %+v, want one", nt.Steps)
				}
				checkStepTiming(t, nt.Steps[0], store.StepTiming{StepID: sn, Succeeded: 3, Measured: 1, MeasuredSeconds: 60})
			})

			t.Run("retried running task", func(t *testing.T) {
				rt := got[r.Job.ID]
				if len(rt.Steps) != 1 {
					t.Fatalf("Steps = %+v, want one", rt.Steps)
				}
				checkStepTiming(t, rt.Steps[0], store.StepTiming{StepID: sr, Running: 1})
				checkRunning(t, rt.Running, sr, t1, nil)
			})

			t.Run("empty input", func(t *testing.T) {
				for _, in := range [][]string{nil, {}} {
					m, err := st.JobTimings(ctx, in)
					if err != nil || m == nil || len(m) != 0 {
						t.Fatalf("JobTimings(%v) = (%v, %v), want empty non-nil map", in, m, err)
					}
				}
			})
		})
	}
}

func TestParity_QueueJobTimings(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			oneTask := stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusReady}}
			running := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning}, oneTask)
			paused := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusPaused, share: &running}, oneTask)
			blocked := blockedOn(t, st, running)
			seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCompleted, share: &running},
				stepSpec{name: "a", status: store.StepStatusCompleted, tasks: []store.TaskStatus{store.TaskStatusSucceeded}})
			seedGraph(t, st, graphOpts{jobStatus: store.JobStatusCanceled, share: &running},
				stepSpec{name: "a", status: store.StepStatusCanceled, tasks: []store.TaskStatus{store.TaskStatusCanceled}})
			elsewhere := seedGraph(t, st, graphOpts{}, oneTask)

			got, err := st.QueueJobTimings(ctx, []string{running.Queue.ID})
			if err != nil {
				t.Fatalf("QueueJobTimings: %v", err)
			}
			var ids []string
			for _, jt := range got {
				ids = append(ids, jt.JobID)
				if jt.QueueID != running.Queue.ID {
					t.Errorf("job %s QueueID = %s, want %s", jt.JobID, jt.QueueID, running.Queue.ID)
				}
				if len(jt.Steps) != 1 || jt.Steps[0].NotStarted != 1 {
					t.Errorf("job %s Steps = %+v, want one step with one task not started", jt.JobID, jt.Steps)
				}
			}
			slices.Sort(ids)
			want := []string{running.Job.ID, paused.Job.ID, blocked.Job.ID}
			slices.Sort(want)
			if !slices.Equal(ids, want) {
				t.Fatalf("jobs = %v, want running, paused and blocked %v", ids, want)
			}

			both, err := st.QueueJobTimings(ctx, []string{running.Queue.ID, elsewhere.Queue.ID})
			if err != nil || len(both) != 4 {
				t.Fatalf("two queues = (%d jobs, %v), want 4", len(both), err)
			}

			for _, in := range [][]string{nil, {}} {
				if got, err := st.QueueJobTimings(ctx, in); err != nil || got != nil {
					t.Fatalf("QueueJobTimings(%v) = (%v, %v), want nil", in, got, err)
				}
			}
		})
	}
}

// TestParity_JobTimingsLatestAttempt: only a task's latest attempt is
// measured or reported running, and two attempts sharing the highest number
// resolve to the greater ID on every backend.
func TestParity_JobTimingsLatestAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farmID, queueID := timingScope(t, st)
			t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
			at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
			inject := func(id, taskID string, num int, status store.AttemptStatus, start time.Duration, end *time.Time) {
				t.Helper()
				storetest.InjectAttempt(t, st, store.TaskAttempt{
					ID: id, TaskID: taskID, WorkerID: fixtureWorkerID, AttemptNumber: num,
					Status: status, StartedAt: t0.Add(start), EndedAt: end,
				})
			}

			step := "s-" + uuid.NewString()
			j := storetest.NewJob(uuid.NewString(), farmID, queueID).As(store.JobStatusRunning).
				Step(step, store.StepStatusReady).
				Task("retried", step, store.TaskStatusReady).
				Task("superseded", step, store.TaskStatusSucceeded).
				Task("tied", step, store.TaskStatusSucceeded).
				Task("tied-run", step, store.TaskStatusReady).
				Submit(t, st)

			// Failed, retried, then succeeded in 45 s: measured once, by the
			// succeeded attempt.
			storetest.FailAndRequeue(t, st,
				store.LeaseRequest{TaskID: "retried", WorkerID: fixtureWorkerID, Now: t0},
				t0.Add(time.Minute))
			ranFor(t, st, "retried", t0.Add(2*time.Minute), 45*time.Second)

			// Succeeded by attempt 1, superseded by attempt 2: not measured.
			inject("sup-1-"+uuid.NewString(), "superseded", 1, store.AttemptStatusSucceeded, 0, at(30*time.Second))
			inject("sup-2-"+uuid.NewString(), "superseded", 2, store.AttemptStatusFailed, time.Minute, at(2*time.Minute))

			// Two succeeded attempts numbered 1: the greater ID (70 s) wins.
			inject("tie-a-"+uuid.NewString(), "tied", 1, store.AttemptStatusSucceeded, 0, at(20*time.Second))
			inject("tie-b-"+uuid.NewString(), "tied", 1, store.AttemptStatusSucceeded, 0, at(70*time.Second))

			// A running task whose leased attempt shares its number with an
			// injected one of greater ID: reported once, from that attempt.
			storetest.Running(t, st, store.LeaseRequest{TaskID: "tied-run", WorkerID: fixtureWorkerID, Now: t0})
			inject("zzzz-"+uuid.NewString(), "tied-run", 1, store.AttemptStatusRunning, 5*time.Minute, nil)

			got, err := st.JobTimings(t.Context(), []string{j.Job.ID})
			if err != nil {
				t.Fatalf("JobTimings: %v", err)
			}
			jt := got[j.Job.ID]
			if len(jt.Steps) != 1 {
				t.Fatalf("Steps = %+v, want one", jt.Steps)
			}
			checkStepTiming(t, jt.Steps[0], store.StepTiming{
				StepID: step, Running: 1, Succeeded: 3, Measured: 2, MeasuredSeconds: 115,
			})
			checkRunning(t, jt.Running, step, t0.Add(5*time.Minute), nil)
		})
	}
}
