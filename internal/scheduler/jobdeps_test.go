// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for jobdeps.go — cross-job (whole-job) dependency reconciliation.
//
// White-box tests in package scheduler, using the fake store and the
// retryNotifier capturing notifier already defined in retry_test.go.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	fakestore "github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── test harness ────────────────────────────────────────────────────────────

// jobDepsHarness bundles a Scheduler wired over a fresh fake store with a
// capturing notifier, plus a pre-created farm and queue for seed helpers.
type jobDepsHarness struct {
	sched   *Scheduler
	store   *fakestore.Store
	notif   *retryNotifier
	farmID  string
	queueID string

	// task/attempt back the single-task job built by seedRunnableJob, for
	// completeJob to report succeeded on.
	task    store.Task
	attempt store.TaskAttempt
}

// newJobDepsHarness builds a Scheduler safe for calling ReconcileDependents /
// sweepBlockedJobs without ever calling Run (bus is nil; jobdeps.go never
// touches it).
func newJobDepsHarness(t *testing.T) *jobDepsHarness {
	t.Helper()
	st := fakestore.New()
	n := &retryNotifier{}
	sched := New(
		DefaultConfig(),
		st,
		nil, // bus — not required by jobdeps.go
		nil, // metrics — not used
		slog.New(slog.DiscardHandler),
		n,
		nil, // diagBuf — diagnostics disabled
	)

	ctx := context.Background()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "f"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "q"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	return &jobDepsHarness{sched: sched, store: st, notif: n, farmID: farm.ID, queueID: queue.ID}
}

// seedJob creates a job with the given status in the harness's farm/queue.
func (h *jobDepsHarness) seedJob(t *testing.T, status store.JobStatus) store.Job {
	t.Helper()
	job, err := h.store.CreateJob(context.Background(), store.Job{
		ID:             uuid.NewString(),
		FarmID:         h.farmID,
		QueueID:        h.queueID,
		Name:           "job-" + uuid.NewString(),
		Status:         status,
		TemplateFormat: store.TemplateFormatJSON,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return job
}

// seedJobInNewQueue creates a job (with the given status) in its own new
// queue within the harness's farm, so tests can prove reconciliation works
// across queues.
func (h *jobDepsHarness) seedJobInNewQueue(t *testing.T, status store.JobStatus) store.Job {
	t.Helper()
	ctx := context.Background()
	queue, err := h.store.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: h.farmID, Name: "q2"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	job, err := h.store.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         h.farmID,
		QueueID:        queue.ID,
		Name:           "job-" + uuid.NewString(),
		Status:         status,
		TemplateFormat: store.TemplateFormatJSON,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return job
}

// seedBlockedJobDependingOn creates a blocked job with one step (pending, no
// intra-job dependencies) and one pending task, then records a cross-job
// dependency edge on each of upstreamIDs.
func (h *jobDepsHarness) seedBlockedJobDependingOn(t *testing.T, upstreamIDs ...string) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         h.farmID,
		QueueID:        h.queueID,
		Name:           "down-" + uuid.NewString(),
		Status:         store.JobStatusBlocked,
		TemplateFormat: store.TemplateFormatJSON,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := h.store.CreateStep(ctx, store.Step{
		ID:     uuid.NewString(),
		JobID:  job.ID,
		Name:   "s1",
		Status: store.StepStatusPending,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if _, err := h.store.CreateTask(ctx, store.Task{
		ID:     uuid.NewString(),
		JobID:  job.ID,
		StepID: step.ID,
		Name:   "t1",
		Status: store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := h.store.CreateJobDependencies(ctx, job.ID, upstreamIDs); err != nil {
		t.Fatalf("CreateJobDependencies: %v", err)
	}
	return job
}

// seedRunnableJob creates a real job with a single running step, a single
// running task, and an open attempt on it — the fixture needed to drive the
// job to completion through the same status-handling path a worker uses (see
// completeJob), rather than mutating job status directly.
func (h *jobDepsHarness) seedRunnableJob(t *testing.T) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         h.farmID,
		QueueID:        h.queueID,
		Name:           "up-" + uuid.NewString(),
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := h.store.CreateStep(ctx, store.Step{
		ID:     uuid.NewString(),
		JobID:  job.ID,
		Name:   "s1",
		Status: store.StepStatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	task, err := h.store.CreateTask(ctx, store.Task{
		ID:     uuid.NewString(),
		JobID:  job.ID,
		StepID: step.ID,
		Name:   "t1",
		Status: store.TaskStatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	h.attempt, err = h.store.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:            uuid.NewString(),
		TaskID:        task.ID,
		AttemptNumber: 1,
		Status:        store.AttemptStatusRunning,
		StartedAt:     time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	h.task = task
	return job
}

// completeJob drives the job seeded by seedRunnableJob to succeeded via the
// same handler a worker's "succeeded" task.status message reaches
// (handleTaskStatusMessage → processTaskStatus → handleTaskTerminal →
// checkStepCompletion → checkJobCompletion), exercising the real completion
// path rather than calling checkJobCompletion in isolation.
func (h *jobDepsHarness) completeJob(t *testing.T, jobID string) {
	t.Helper()
	if h.sched.ctx == nil {
		h.sched.ctx = context.Background()
	}
	exitCode := 0
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    h.task.ID,
			AttemptID: h.attempt.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	h.sched.handleTaskStatusMessage(msg)
	if !msg.acked {
		t.Fatalf("completeJob(%s): expected task.status message to be acked", jobID)
	}
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestReconcile_ReleasesWhenAllUpstreamsCompleted(t *testing.T) {
	ctx := context.Background()
	h := newJobDepsHarness(t)

	// Upstream lives in a different queue than the dependent, proving
	// reconciliation is cross-queue.
	up := h.seedJobInNewQueue(t, store.JobStatusRunning)
	down := h.seedBlockedJobDependingOn(t, up.ID)

	if err := h.store.UpdateJobStatus(ctx, up.ID, store.JobStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := h.sched.ReconcileDependents(ctx, up.ID); err != nil {
		t.Fatal(err)
	}

	got, err := h.store.GetJob(ctx, down.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.JobStatusPending {
		t.Fatalf("down status = %q, want pending (released)", got.Status)
	}

	page, err := h.store.ListTasks(ctx, store.ListTasksOptions{JobID: down.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		t.Fatal("expected at least one task")
	}
	for _, tk := range page.Items {
		if tk.Status != store.TaskStatusReady {
			t.Fatalf("task %s status = %q, want ready", tk.Name, tk.Status)
		}
	}
}

func TestReconcile_CancelsWhenUpstreamFailed(t *testing.T) {
	ctx := context.Background()
	h := newJobDepsHarness(t)
	up := h.seedJob(t, store.JobStatusRunning)
	down := h.seedBlockedJobDependingOn(t, up.ID)

	if err := h.store.UpdateJobStatus(ctx, up.ID, store.JobStatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := h.sched.ReconcileDependents(ctx, up.ID); err != nil {
		t.Fatal(err)
	}

	got, err := h.store.GetJob(ctx, down.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.JobStatusCanceled {
		t.Fatalf("down status = %q, want canceled", got.Status)
	}

	page, err := h.store.ListTasks(ctx, store.ListTasksOptions{JobID: down.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		t.Fatal("expected at least one task")
	}
	for _, tk := range page.Items {
		if tk.Status != store.TaskStatusCanceled {
			t.Fatalf("task %s status = %q, want canceled", tk.Name, tk.Status)
		}
		if tk.FailureReason != store.FailureReasonUpstreamFailed {
			t.Fatalf("task %s failure reason = %q, want %q", tk.Name, tk.FailureReason, store.FailureReasonUpstreamFailed)
		}
	}
}

func TestReconcile_CancelsWhenUpstreamDeleted(t *testing.T) {
	ctx := context.Background()
	h := newJobDepsHarness(t)
	up := h.seedJob(t, store.JobStatusRunning)
	down := h.seedBlockedJobDependingOn(t, up.ID)

	if err := h.store.DeleteJob(ctx, up.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.sched.ReconcileDependents(ctx, up.ID); err != nil {
		t.Fatal(err)
	}

	got, err := h.store.GetJob(ctx, down.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.JobStatusCanceled {
		t.Fatalf("down status = %q, want canceled (upstream deleted)", got.Status)
	}
}

func TestReconcile_FanInWaitsForLastUpstream(t *testing.T) {
	ctx := context.Background()
	h := newJobDepsHarness(t)
	up1 := h.seedJob(t, store.JobStatusRunning)
	up2 := h.seedJob(t, store.JobStatusRunning)
	down := h.seedBlockedJobDependingOn(t, up1.ID, up2.ID)

	if err := h.store.UpdateJobStatus(ctx, up1.ID, store.JobStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := h.sched.ReconcileDependents(ctx, up1.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := h.store.GetJob(ctx, down.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.JobStatusBlocked {
		t.Fatalf("after 1/2 upstreams: status = %q, want still blocked", got.Status)
	}

	if err := h.store.UpdateJobStatus(ctx, up2.ID, store.JobStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := h.sched.ReconcileDependents(ctx, up2.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := h.store.GetJob(ctx, down.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.JobStatusPending {
		t.Fatalf("after 2/2 upstreams: status = %q, want pending", got.Status)
	}
}

func TestSweepBlockedJobs_ReleasesAndCancels(t *testing.T) {
	ctx := context.Background()
	h := newJobDepsHarness(t)

	// down1 releases: its upstream is already completed before the sweep runs.
	up1 := h.seedJob(t, store.JobStatusCompleted)
	down1 := h.seedBlockedJobDependingOn(t, up1.ID)

	// down2 cancels: its upstream already failed before the sweep runs.
	up2 := h.seedJob(t, store.JobStatusFailed)
	down2 := h.seedBlockedJobDependingOn(t, up2.ID)

	if err := h.sched.sweepBlockedJobs(ctx); err != nil {
		t.Fatal(err)
	}

	if got, err := h.store.GetJob(ctx, down1.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.JobStatusPending {
		t.Fatalf("sweep: down1 status = %q, want pending", got.Status)
	}
	if got, err := h.store.GetJob(ctx, down2.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.JobStatusCanceled {
		t.Fatalf("sweep: down2 status = %q, want canceled", got.Status)
	}
}

func TestReconcile_CascadesTransitivelyThroughChain(t *testing.T) {
	// up (fails) -> mid (blocked on up) -> leaf (blocked on mid): the leaf
	// must also end up canceled even though its own upstream (mid) never
	// itself transitions via UpdateJobStatus — cancelAndCascade must drive it.
	ctx := context.Background()
	h := newJobDepsHarness(t)
	up := h.seedJob(t, store.JobStatusRunning)
	mid := h.seedBlockedJobDependingOn(t, up.ID)
	leaf := h.seedBlockedJobDependingOn(t, mid.ID)

	if err := h.store.UpdateJobStatus(ctx, up.ID, store.JobStatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := h.sched.ReconcileDependents(ctx, up.ID); err != nil {
		t.Fatal(err)
	}

	if got, err := h.store.GetJob(ctx, mid.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.JobStatusCanceled {
		t.Fatalf("mid status = %q, want canceled", got.Status)
	}
	if got, err := h.store.GetJob(ctx, leaf.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.JobStatusCanceled {
		t.Fatalf("leaf status = %q, want canceled (cascaded transitively)", got.Status)
	}
}

func TestReconcile_NotifiesJobAndTaskEvents(t *testing.T) {
	ctx := context.Background()
	h := newJobDepsHarness(t)
	up := h.seedJob(t, store.JobStatusRunning)
	down := h.seedBlockedJobDependingOn(t, up.ID)

	if err := h.store.UpdateJobStatus(ctx, up.ID, store.JobStatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := h.sched.ReconcileDependents(ctx, up.ID); err != nil {
		t.Fatal(err)
	}

	if h.notif.JobEventCount() == 0 {
		t.Error("expected at least one job event notification")
	}
	if h.notif.TaskEventCount() == 0 {
		t.Error("expected at least one task event notification")
	}

	h.notif.mu.Lock()
	defer h.notif.mu.Unlock()
	found := false
	for _, e := range h.notif.jobEvents {
		if e.JobID == down.ID && e.Status == string(store.JobStatusCanceled) {
			found = true
		}
	}
	if !found {
		t.Error("expected a job event for the canceled dependent job")
	}
}

// TestCheckJobCompletion_ReleasesDependent proves the completion hook is
// wired end-to-end: driving a real upstream job to completion through the
// same status handler a worker's succeeded task.status message reaches
// (handleTaskStatusMessage, via completeJob) must release a dependent job
// blocked on it — without any direct call to ReconcileDependents.
func TestCheckJobCompletion_ReleasesDependent(t *testing.T) {
	ctx := context.Background()
	h := newJobDepsHarness(t)
	up := h.seedRunnableJob(t)
	down := h.seedBlockedJobDependingOn(t, up.ID)

	h.completeJob(t, up.ID)

	gotUp, err := h.store.GetJob(ctx, up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUp.Status != store.JobStatusCompleted {
		t.Fatalf("upstream status = %q, want completed", gotUp.Status)
	}

	gotDown, err := h.store.GetJob(ctx, down.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDown.Status != store.JobStatusPending {
		t.Fatalf("down status = %q, want pending after upstream completion", gotDown.Status)
	}
}
