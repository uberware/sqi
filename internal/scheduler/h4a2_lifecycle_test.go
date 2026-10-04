// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// TestH4a2_CancelLastOpenTaskFinalizesStepAndJob pins item 9 i: canceling a
// job's last open task finishes its step and its job, as a terminal worker
// report does. Before H4a2 the step stayed open until the next restart. The
// step already holds a succeeded sibling, so the cancel of the last OPEN task is
// what ends it, and it ends canceled (a canceled task and no failed one).
func TestH4a2_CancelLastOpenTaskFinalizesStepAndJob(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, step, task, _ := seedStatusFixture(t, st, store.TaskStatusRunning)
			now := time.Now()
			if _, err := st.CreateTask(t.Context(), store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "task-sibling",
				Status: store.TaskStatusSucceeded, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("CreateTask (succeeded sibling): %v", err)
			}
			notifier := &jobRecordingNotifier{}
			s := New(
				DefaultConfig(), st, &stubBus{}, nil,
				slog.New(slog.DiscardHandler), notifier, nil,
			)
			if err := s.CancelTask(t.Context(), task.ID); err != nil {
				t.Fatalf("CancelTask: %v", err)
			}
			if got := mustStep(t, st, step.ID).Status; got != store.StepStatusCanceled {
				t.Fatalf("step = %q, want canceled", got)
			}
			if got := mustJob(t, st, job.ID).Status; got != store.JobStatusCanceled {
				t.Fatalf("job = %q, want canceled", got)
			}
			if !notifier.hasJobStatus(job.ID, string(store.JobStatusCanceled)) {
				t.Fatalf("no canceled job event was emitted; got %+v", notifier.jobs)
			}
		})
	}
}

// TestH4a2_CancelJobThenRetryJobRunsAgain pins items 9 ii and v: a job
// canceled with nothing in flight, then retried, has its tasks released and
// leasable. Before H4a2 the job cancel left its step open, RetryTasks reset
// only failed/canceled steps, and the revived task stayed pending forever.
func TestH4a2_CancelJobThenRetryJobRunsAgain(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, step, task, _ := seedStatusFixture(t, st, store.TaskStatusReady)
			s := newTestScheduler(st, &stubBus{})
			if err := s.CancelJob(t.Context(), job.ID); err != nil {
				t.Fatalf("CancelJob: %v", err)
			}
			if err := st.CancelJobStatus(t.Context(), job.ID); err != nil {
				t.Fatalf("CancelJobStatus: %v", err)
			}
			n, err := s.RetryJob(t.Context(), job.ID)
			if err != nil || n != 1 {
				t.Fatalf("RetryJob = (%d, %v), want (1, nil)", n, err)
			}
			if got := mustTaskOf(t, st, task.ID).Status; got != store.TaskStatusReady {
				t.Fatalf("task = %q, want ready (leasable)", got)
			}
			if got := mustStep(t, st, step.ID).Status; got != store.StepStatusReady {
				t.Fatalf("step = %q, want ready", got)
			}
			if got := mustJob(t, st, job.ID).Status; got != store.JobStatusPending {
				t.Fatalf("job = %q, want pending", got)
			}
		})
	}
}

// TestH4a2_CancelEchoAfterJobCancelKeepsTheJobCanceled pins the final review's
// cancel-echo race. A job cancel finalizes every step, so a step holding a
// failed task ends failed. When the job row was a second write, the worker's
// "canceled" echo for a canceled task could reach checkStepCompletion before it,
// and FinalizeJob then ended the job failed: the user got a 409 although every
// task had been canceled. The job row is now canceled in the cancel's own
// transaction, so the echo finds a terminal job and the confirmation succeeds.
func TestH4a2_CancelEchoAfterJobCancelKeepsTheJobCanceled(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
			now := time.Now()
			bad, err := st.CreateStep(t.Context(), store.Step{
				ID: uuid.NewString(), JobID: job.ID, Name: "Bad", StepOrder: 1,
				Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateStep: %v", err)
			}
			if _, err := st.CreateTask(t.Context(), store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: bad.ID, Name: "t-failed",
				Status: store.TaskStatusFailed, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("CreateTask (failed): %v", err)
			}
			s := newTestScheduler(st, &stubBus{})
			s.ctx = t.Context()
			if err := s.CancelJob(t.Context(), job.ID); err != nil {
				t.Fatalf("CancelJob: %v", err)
			}
			// The worker's echo of the cancel, a same-status terminal report.
			msg := terminalReport(t, task, attempt, "canceled", "")
			s.handleTaskStatusMessage(msg)
			if !msg.acked || msg.nacked {
				t.Fatalf("echo must be acked (acked=%v nacked=%v)", msg.acked, msg.nacked)
			}
			if got := mustJob(t, st, job.ID).Status; got != store.JobStatusCanceled {
				t.Fatalf("job = %q after the cancel echo, want canceled", got)
			}
			if err := st.CancelJobStatus(t.Context(), job.ID); err != nil {
				t.Fatalf("CancelJobStatus = %v, want nil (the job is already canceled)", err)
			}
		})
	}
}

// TestH4a2_RetryFailedTaskWhileSiblingRunsIsLeasable pins item 9 vi through
// RetryTask: the revived task is ready at once, not pending under a ready step.
func TestH4a2_RetryFailedTaskWhileSiblingRunsIsLeasable(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, step, running, _ := seedStatusFixture(t, st, store.TaskStatusRunning)
			now := time.Now()
			failed, err := st.CreateTask(t.Context(), store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t-failed",
				Status: store.TaskStatusFailed, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			s := newTestScheduler(st, &stubBus{})
			if err := s.RetryTask(t.Context(), failed.ID); err != nil {
				t.Fatalf("RetryTask: %v", err)
			}
			if got := mustTaskOf(t, st, failed.ID).Status; got != store.TaskStatusReady {
				t.Fatalf("retried task = %q, want ready", got)
			}
			if got := mustTaskOf(t, st, running.ID).Status; got != store.TaskStatusRunning {
				t.Fatalf("sibling = %q, want still running", got)
			}
		})
	}
}

// fixtureStepStatus is the fixture-only blind step write both concrete stores
// keep after it left store.Store (H4a).
type fixtureStepStatus interface {
	UpdateStepStatus(ctx context.Context, id string, status store.StepStatus) error
}

func forceStepStatus(st store.Store, stepID string, status store.StepStatus) error {
	f, ok := st.(fixtureStepStatus)
	if !ok {
		return fmt.Errorf("%T has no fixture UpdateStepStatus", st)
	}
	return f.UpdateStepStatus(context.Background(), stepID, status)
}

// TestH4a2_StartupReleasesAStrandedPendingStep pins spec §3.5 / D5: a step a
// retry reset to pending and never released (the server stopped first) is
// released, with its task, at the next start. A blocked job that also holds a
// pending, dependency-free step is left alone: ResolveDependencies does not
// look at job status, so only the store's exclusion of blocked jobs keeps that
// step from being released early.
func TestH4a2_StartupReleasesAStrandedPendingStep(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, step, task, _ := seedStatusFixture(t, st, store.TaskStatusPending)
			if err := forceStepStatus(st, step.ID, store.StepStatusPending); err != nil {
				t.Fatalf("force step pending: %v", err)
			}

			now := time.Now()
			blocked, err := st.CreateJob(t.Context(), store.Job{
				ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: "blocked",
				Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON,
				CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateJob (blocked): %v", err)
			}
			blockedStep, err := st.CreateStep(t.Context(), store.Step{
				ID: uuid.NewString(), JobID: blocked.ID, Name: "Step1",
				Status: store.StepStatusPending, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateStep (blocked): %v", err)
			}
			blockedTask, err := st.CreateTask(t.Context(), store.Task{
				ID: uuid.NewString(), JobID: blocked.ID, StepID: blockedStep.ID, Name: "task-0",
				Status: store.TaskStatusPending, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTask (blocked): %v", err)
			}

			s := newTestScheduler(st, &stubBus{})
			s.reconcilePendingSteps(t.Context())

			if got := mustStep(t, st, step.ID).Status; got != store.StepStatusReady {
				t.Fatalf("stranded step = %q, want ready", got)
			}
			if got := mustTaskOf(t, st, task.ID).Status; got != store.TaskStatusReady {
				t.Fatalf("stranded task = %q, want ready", got)
			}
			if got := mustJob(t, st, blocked.ID).Status; got != store.JobStatusBlocked {
				t.Fatalf("blocked job = %q, want blocked (untouched)", got)
			}
			if got := mustStep(t, st, blockedStep.ID).Status; got != store.StepStatusPending {
				t.Fatalf("blocked job's step = %q, want pending (untouched)", got)
			}
			if got := mustTaskOf(t, st, blockedTask.ID).Status; got != store.TaskStatusPending {
				t.Fatalf("blocked job's task = %q, want pending (untouched)", got)
			}
		})
	}
}

// TestH4a2_StartupCancelsAndFinalizesBehindAFailedUpstream pins the other half
// of the start-up pass: a pending step behind an upstream that already failed
// can never run, so it is cascade-canceled with its task, and with no step left
// open the job is finalized (failed) rather than left running.
func TestH4a2_StartupCancelsAndFinalizesBehindAFailedUpstream(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, upstream, _, _ := seedStatusFixture(t, st, store.TaskStatusFailed)
			if err := forceStepStatus(st, upstream.ID, store.StepStatusFailed); err != nil {
				t.Fatalf("force upstream failed: %v", err)
			}
			now := time.Now()
			down, err := st.CreateStep(t.Context(), store.Step{
				ID: uuid.NewString(), JobID: job.ID, Name: "Step2", DependsOn: []string{upstream.Name},
				Status: store.StepStatusPending, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateStep (downstream): %v", err)
			}
			downTask, err := st.CreateTask(t.Context(), store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: down.ID, Name: "task-0",
				Status: store.TaskStatusPending, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTask (downstream): %v", err)
			}

			s := newTestScheduler(st, &stubBus{})
			s.reconcilePendingSteps(t.Context())

			if got := mustStep(t, st, down.ID).Status; got != store.StepStatusCanceled {
				t.Fatalf("downstream step = %q, want canceled", got)
			}
			if got := mustTaskOf(t, st, downTask.ID).Status; got != store.TaskStatusCanceled {
				t.Fatalf("downstream task = %q, want canceled", got)
			}
			if got := mustJob(t, st, job.ID).Status; got != store.JobStatusFailed {
				t.Fatalf("job = %q, want failed (no step left open)", got)
			}
		})
	}
}
