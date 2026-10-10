// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for sweepUnschedulable in unschedulable.go: the heartbeat-sweep pass
// that flags ready tasks no online worker can satisfy (and clears the flag
// once a matching worker appears). White-box tests in package scheduler,
// driven by a fake store; mirrors the fixture style used by
// heartbeat_sweep_test.go and assignment_test.go (newMetricsScheduler,
// recordBus) rather than inventing a parallel harness.

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// seedReadyTaskRequiringTag creates a farm/queue/job/step/ready-task in
// "farm-1" whose step requires the worker tag attr.worker.tag.<tag>="true".
// Returns the created job, step, and task.
func seedReadyTaskRequiringTag(t *testing.T, st *fake.Store, tag string) (store.Job, store.Step, store.Task) {
	t.Helper()
	ctx := t.Context()

	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "queue"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	jobID, stepID := uuid.NewString(), uuid.NewString()
	sub := storetest.Submit(t, st, store.JobSubmission{
		Job: store.Job{
			ID: jobID, FarmID: "farm-1", QueueID: "queue-1", Name: "j",
			Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		},
		Steps: []store.Step{{
			ID: stepID, JobID: jobID, Name: "s",
			Status: store.StepStatusRunning,
			HostRequirements: &store.StepHostRequirements{
				Attributes: []store.StepAttributeRequirement{
					{Name: "attr.worker.tag." + tag, AnyOf: []string{"true"}},
				},
			},
		}},
		Tasks: []store.Task{{
			ID: uuid.NewString(), JobID: jobID, StepID: stepID, Name: "t",
			Status: store.TaskStatusReady,
		}},
	})

	return sub.Job, sub.Steps[0], sub.Tasks[0]
}

// unschedulableWorkerSeq gives each seedOnlineWorker call a unique worker ID
// within a test.
var unschedulableWorkerSeq int

// seedOnlineWorker registers a fresh online worker in "farm-1" carrying the
// given capability tags.
func seedOnlineWorker(t *testing.T, st *fake.Store, tags map[string]string) store.Worker {
	t.Helper()
	unschedulableWorkerSeq++
	id := fmt.Sprintf("w-unsched-%d", unschedulableWorkerSeq)
	w, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: id, FarmID: "farm-1", Hostname: id, Tags: tags,
		Status: store.WorkerStatusOnline,
	})
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	return w
}

// backdateTaskReady ages taskID's UpdatedAt by delta (negative to move it into
// the past). It does so through the writes a worker that took the task and
// then shut down makes: the task is leased with the lease's Now delta from
// now, and that attempt is reclaimed at the same instant, which returns the
// task to ready stamped with that time — a task that has been waiting since.
func backdateTaskReady(t *testing.T, st *fake.Store, taskID string, delta time.Duration) {
	t.Helper()
	at := time.Now().UTC().Add(delta)
	attempt := storetest.Lease(t, st, store.LeaseRequest{TaskID: taskID, WorkerID: "w-gone", Now: at})
	if reclaimed, err := st.ReclaimTaskAttempt(t.Context(), attempt.ID, taskID, at); err != nil || !reclaimed {
		t.Fatalf("ReclaimTaskAttempt = (%v, %v), want reclaimed", reclaimed, err)
	}
	task, err := st.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusReady || !task.UpdatedAt.Equal(at) {
		t.Fatalf("backdated task = %q updated %v, want ready updated %v", task.Status, task.UpdatedAt, at)
	}
}

// TestSweepUnschedulable_FlagsAndClears verifies that a ready task past grace
// with no eligible online worker gets flagged, and that the flag clears on the
// next sweep once a matching worker registers.
func TestSweepUnschedulable_FlagsAndClears(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	s.cfg.UnschedulableGrace = time.Millisecond

	_, _, task := seedReadyTaskRequiringTag(t, st, "maya")
	seedOnlineWorker(t, st, map[string]string{}) // no maya tag
	backdateTaskReady(t, st, task.ID, -time.Minute)

	s.sweepUnschedulable(t.Context())

	got, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.UnschedulableReason == "" {
		t.Fatalf("expected unschedulable reason, got empty")
	}

	// Now add a matching worker — next sweep clears it.
	seedOnlineWorker(t, st, map[string]string{"maya": "true"})
	s.sweepUnschedulable(t.Context())

	got, err = st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.UnschedulableReason != "" {
		t.Errorf("expected cleared reason, got %q", got.UnschedulableReason)
	}
}

// TestSweepUnschedulable_DisabledWhenGraceZero verifies that UnschedulableGrace
// <= 0 disables the sweep entirely — an unschedulable task past any age is left
// untouched.
func TestSweepUnschedulable_DisabledWhenGraceZero(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	s.cfg.UnschedulableGrace = 0

	_, _, task := seedReadyTaskRequiringTag(t, st, "maya")
	seedOnlineWorker(t, st, map[string]string{})
	backdateTaskReady(t, st, task.ID, -time.Minute)

	s.sweepUnschedulable(t.Context())

	got, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.UnschedulableReason != "" {
		t.Errorf("grace=0 must disable sweep, got %q", got.UnschedulableReason)
	}
}

// TestSweepUnschedulable_WithinGrace_NotYetFlagged verifies that a ready task
// still within the grace window is left alone even though no worker is
// eligible yet — avoids flapping the flag on freshly-submitted work.
func TestSweepUnschedulable_WithinGrace_NotYetFlagged(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	s.cfg.UnschedulableGrace = time.Hour

	_, _, task := seedReadyTaskRequiringTag(t, st, "maya")
	seedOnlineWorker(t, st, map[string]string{})
	// No backdating: task.UpdatedAt is "now", well within the 1h grace window.

	s.sweepUnschedulable(t.Context())

	got, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.UnschedulableReason != "" {
		t.Errorf("task within grace window should not be flagged, got %q", got.UnschedulableReason)
	}
}

// TestSweepUnschedulable_NoOnlineWorkers verifies the "no online workers"
// reason is used when the farm has no online workers at all.
func TestSweepUnschedulable_NoOnlineWorkers(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	s.cfg.UnschedulableGrace = time.Millisecond

	_, _, task := seedReadyTaskRequiringTag(t, st, "maya")
	backdateTaskReady(t, st, task.ID, -time.Minute)

	s.sweepUnschedulable(t.Context())

	got, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.UnschedulableReason != "no online workers" {
		t.Errorf("reason = %q, want %q", got.UnschedulableReason, "no online workers")
	}
}
