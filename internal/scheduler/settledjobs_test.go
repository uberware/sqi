// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/ws"
)

// settledStep is one terminal step holding one task of the matching status.
func settledStep(name string, status store.StepStatus) statusStep {
	task := map[store.StepStatus]store.TaskStatus{
		store.StepStatusCompleted: store.TaskStatusSucceeded,
		store.StepStatusFailed:    store.TaskStatusFailed,
		store.StepStatusCanceled:  store.TaskStatusCanceled,
	}[status]
	return statusStep{name: name, status: status, tasks: []store.TaskStatus{task}}
}

func TestFinalizeSettledJobs_FinalizesAndReleasesDependents(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			upstream := seedStatusJob(t, st, statusJob{steps: []statusStep{
				settledStep("A", store.StepStatusCompleted), settledStep("B", store.StepStatusCompleted),
			}})
			dependent := newReconcileJob("dependent", store.JobStatusBlocked)
			depStep := newReconcileStep(dependent.ID, "S", 0, store.StepStatusPending)
			storetest.Submit(t, st, store.JobSubmission{
				Job: dependent, DependsOn: []string{upstream.job.ID}, Steps: []store.Step{depStep},
				Tasks: reconcileTasks(dependent.ID, depStep.ID, store.TaskStatusPending),
			})
			events := &reconcileEvents{}
			s := newReconcileScheduler(st, &recordBus{}, events)

			s.finalizeSettledJobs(t.Context())

			wantJobStatus(t, st, upstream.job.ID, "upstream", store.JobStatusCompleted)
			jobs, _ := events.snapshot()
			var upstreamEvents []ws.JobEvent
			for _, e := range jobs {
				if e.JobID == upstream.job.ID {
					upstreamEvents = append(upstreamEvents, e)
				}
			}
			if len(upstreamEvents) != 1 || upstreamEvents[0].Status != string(store.JobStatusCompleted) {
				t.Errorf("upstream job events = %+v, want one completed event", upstreamEvents)
			}
			if got := mustJob(t, st, dependent.ID).Status; got == store.JobStatusBlocked {
				t.Errorf("dependent job is still blocked after its upstream was finalized")
			}
		})
	}
}

func TestSweepOnce_FinalizesSettledJob(t *testing.T) {
	st := newCheckedFake(t)
	g := seedStatusJob(t, st, statusJob{steps: []statusStep{settledStep("A", store.StepStatusCompleted)}})
	s := newReconcileScheduler(st, &recordBus{}, ws.NoopNotifier{})

	s.sweepOnce(t.Context())

	wantJobStatus(t, st, g.job.ID, "settled", store.JobStatusCompleted)
}

func TestFinalizeSettledJobs_Outcomes(t *testing.T) {
	tests := []struct {
		name string
		spec statusJob
		want store.JobStatus
	}{
		{
			name: "a failed step fails the job",
			spec: statusJob{steps: []statusStep{
				settledStep("A", store.StepStatusCompleted), settledStep("B", store.StepStatusFailed),
			}},
			want: store.JobStatusFailed,
		},
		{
			name: "a paused job is finalized like a running one",
			spec: statusJob{jobStatus: store.JobStatusPaused, steps: []statusStep{
				settledStep("A", store.StepStatusCompleted),
			}},
			want: store.JobStatusCompleted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newCheckedFake(t)
			g := seedStatusJob(t, st, tc.spec)
			s := newReconcileScheduler(st, &recordBus{}, ws.NoopNotifier{})

			s.finalizeSettledJobs(t.Context())

			wantJobStatus(t, st, g.job.ID, tc.name, tc.want)
		})
	}
}

func TestFinalizeSettledJobs_HealthyFarmWritesNothing(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			f := seedHealthyFarm(t, st)
			before := snapshotFarm(t, st, f.jobs)
			spy := &writeCountingStore{Store: st}
			events := &reconcileEvents{}

			newReconcileScheduler(spy, &recordBus{}, events).finalizeSettledJobs(t.Context())

			if n := spy.finalizeJob.Load(); n != 0 {
				t.Errorf("FinalizeJob called %d times on a healthy farm, want 0", n)
			}
			if after := snapshotFarm(t, st, f.jobs); !reflect.DeepEqual(before, after) {
				t.Errorf("a row of a healthy farm changed:\nbefore %+v\nafter  %+v", before, after)
			}
			if jobs, tasks := events.snapshot(); len(jobs) != 0 || len(tasks) != 0 {
				t.Errorf("a healthy farm emitted events: jobs %v, tasks %v", jobs, tasks)
			}
		})
	}
}

// TestFinalizeSettledJobs_StaleListEntryIsLeftAlone covers a job listed as
// settled whose step reopened (a retry) before its FinalizeJob ran.
func TestFinalizeSettledJobs_StaleListEntryIsLeftAlone(t *testing.T) {
	st := newCheckedFake(t)
	g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
		name: "A", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusRunning},
	}}})
	spy := &writeCountingStore{Store: st, settledJobs: []string{g.job.ID}}
	events := &reconcileEvents{}

	newReconcileScheduler(spy, &recordBus{}, events).finalizeSettledJobs(t.Context())

	wantJobStatus(t, st, g.job.ID, "reopened", store.JobStatusRunning)
	if jobs, _ := events.snapshot(); len(jobs) != 0 {
		t.Errorf("job events = %+v, want none", jobs)
	}
}

func TestFinalizeSettledJobs_ErrorOnOneJobDoesNotStopTheNext(t *testing.T) {
	st := newCheckedFake(t)
	spec := statusJob{steps: []statusStep{settledStep("A", store.StepStatusCompleted)}}
	a, b := seedStatusJob(t, st, spec), seedStatusJob(t, st, spec)
	ids := []string{a.job.ID, b.job.ID}
	if ids[0] > ids[1] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	spy := &writeCountingStore{Store: st, failFinalizeJob: ids[0]}

	newReconcileScheduler(spy, &recordBus{}, ws.NoopNotifier{}).finalizeSettledJobs(t.Context())

	wantJobStatus(t, st, ids[0], "failing job", store.JobStatusRunning)
	wantJobStatus(t, st, ids[1], "next job", store.JobStatusCompleted)
}

func TestFinalizeSettledJobs_FencedStops(t *testing.T) {
	st := newCheckedFake(t)
	spec := statusJob{steps: []statusStep{settledStep("A", store.StepStatusCompleted)}}
	a, b := seedStatusJob(t, st, spec), seedStatusJob(t, st, spec)
	spy := &writeCountingStore{Store: st}
	s, rec := newFenceScheduler(t, spy, "farm-1")

	s.finalizeSettledJobs(deposedCtx(t, st))

	if n := spy.finalizeJob.Load(); n != 1 {
		t.Errorf("FinalizeJob called %d times, want 1 (the sweep stops at the fenced write)", n)
	}
	for _, g := range []statusGraph{a, b} {
		wantJobStatus(t, st, g.job.ID, "fenced", store.JobStatusRunning)
	}
	rec.assertFencedOnce(t)
}

// TestSweepOnce_FinalizesStuckStep pins that a sweep tick finalizes a step of a
// live job whose tasks are all terminal but which was never finalized, and the
// job with it, with no restart.
func TestSweepOnce_FinalizesStuckStep(t *testing.T) {
	st := newCheckedFake(t)
	g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
		name: "A", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusSucceeded},
	}}})
	s := newReconcileScheduler(st, &recordBus{}, ws.NoopNotifier{})

	s.sweepOnce(t.Context())

	wantStepStatus(t, st, g.steps[0].ID, "stuck step", store.StepStatusCompleted)
	wantJobStatus(t, st, g.job.ID, "its job", store.JobStatusCompleted)
}

// TestFinalizeSettledJobs_LogsOnlyARepair pins that the sweep reports a repair
// only when its write finalized the job: a listed job whose step reopened is
// left without a "finalized" line, and a listed job deleted before its turn is
// skipped without a warning.
func TestFinalizeSettledJobs_LogsOnlyARepair(t *testing.T) {
	st := newCheckedFake(t)
	reopened := seedStatusJob(t, st, statusJob{steps: []statusStep{{
		name: "A", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusRunning},
	}}})
	spy := &writeCountingStore{Store: st, settledJobs: []string{reopened.job.ID, "job-deleted"}}
	logs := &lockedBuffer{}
	s := newReconcileScheduler(spy, &recordBus{}, ws.NoopNotifier{})
	s.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s.finalizeSettledJobs(t.Context())

	if out := logs.String(); strings.Contains(out, "finalized a live job") || strings.Contains(out, "level=WARN") {
		t.Errorf("sweep logged a repair or a warning for jobs it did not finalize:\n%s", out)
	}
}
