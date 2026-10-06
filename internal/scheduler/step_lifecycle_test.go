// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestCancelLastOpenTaskFinalizesStepAndJob pins that canceling a job's last
// open task finishes its step and its job, as a terminal worker report does,
// rather than leaving the step open until the next restart. The step already
// holds a succeeded sibling, so the cancel of the last open task is what ends
// it, and it ends canceled (a canceled task and no failed one).
func TestCancelLastOpenTaskFinalizesStepAndJob(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
				name: "Step1", status: store.StepStatusRunning,
				tasks: []store.TaskStatus{store.TaskStatusRunning, store.TaskStatusSucceeded}, // a succeeded sibling
			}}})
			job, step, task := g.job, g.steps[0], g.tasks[0][0]
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

// TestCancelJobThenRetryJobRunsAgain pins that a job canceled with nothing in
// flight, then retried, has its tasks released and leasable. If the job cancel
// left its step open, RetryTasks, which resets only failed/canceled steps,
// would leave the revived task pending forever.
func TestCancelJobThenRetryJobRunsAgain(t *testing.T) {
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

// TestCancelEchoAfterJobCancelKeepsTheJobCanceled pins the cancel-echo race. A
// job cancel finalizes every step, so a step holding a failed task ends failed.
// If the job row were a second write, the worker's "canceled" echo for a
// canceled task could reach checkStepCompletion before it, and FinalizeJob
// would end the job failed: the user would get a 409 although every task had
// been canceled. The job row is canceled in the cancel's own transaction, so
// the echo finds a terminal job and the confirmation succeeds.
func TestCancelEchoAfterJobCancelKeepsTheJobCanceled(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStatusJob(t, st, statusJob{steps: []statusStep{
				{name: "Step1", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusRunning}},
				{name: "Bad", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusFailed}},
			}})
			job, task := g.job, g.tasks[0][0]
			attempt := g.attempts[task.ID]
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

// TestRetryFailedTaskWhileSiblingRunsIsLeasable pins, through RetryTask, that
// the revived task is ready at once, not pending under a ready step.
func TestRetryFailedTaskWhileSiblingRunsIsLeasable(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
				name: "Step1", status: store.StepStatusRunning,
				tasks: []store.TaskStatus{store.TaskStatusRunning, store.TaskStatusFailed},
			}}})
			running, failed := g.tasks[0][0], g.tasks[0][1]
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

// TestStartupReleasesAStrandedPendingStep pins that a step a retry reset to
// pending and never released (the server stopped first) is released, with its
// task, at the next start. A blocked job that also holds a pending,
// dependency-free step is left alone: ResolveDependencies does not look at job
// status, so only the store's exclusion of blocked jobs keeps that step from
// being released early.
func TestStartupReleasesAStrandedPendingStep(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			// The step as a retry left it: pending, with its task pending.
			g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
				name: "Step1", status: store.StepStatusPending, tasks: []store.TaskStatus{store.TaskStatusPending},
			}}})
			step, task := g.steps[0], g.tasks[0][0]

			blocked := store.Job{
				ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: "blocked",
				Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON,
			}
			blockedStep := store.Step{ID: uuid.NewString(), JobID: blocked.ID, Name: "Step1", Status: store.StepStatusPending}
			blockedTask := store.Task{
				ID: uuid.NewString(), JobID: blocked.ID, StepID: blockedStep.ID, Name: "task-0", Status: store.TaskStatusPending,
			}
			storetest.Submit(t, st, store.JobSubmission{Job: blocked, Steps: []store.Step{blockedStep}, Tasks: []store.Task{blockedTask}})

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

// TestStartupCancelsAndFinalizesBehindAFailedUpstream pins the other half
// of the start-up pass: a pending step behind an upstream that already failed
// can never run, so it is cascade-canceled with its task, and with no step left
// open the job is finalized (failed) rather than left running.
func TestStartupCancelsAndFinalizesBehindAFailedUpstream(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStatusJob(t, st, statusJob{steps: []statusStep{
				{name: "Step1", status: store.StepStatusFailed, tasks: []store.TaskStatus{store.TaskStatusFailed}},
				{name: "Step2", status: store.StepStatusPending, dependsOn: []string{"Step1"}, tasks: []store.TaskStatus{store.TaskStatusPending}},
			}})
			job, down, downTask := g.job, g.steps[1], g.tasks[1][0]

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

// retryBeforeCascadeStore lands a RetryJob's store write immediately before
// the failure cascade's cancel of stepID: the RetryJob that arrives between the
// upstream step finalizing failed and CancelDependents canceling its dependents.
type retryBeforeCascadeStore struct {
	store.Store

	jobID, stepID string
	fired         bool
	t             *testing.T
}

func (s *retryBeforeCascadeStore) CancelPendingStep(ctx context.Context, id, reason string, now time.Time) (bool, []store.Task, error) {
	if id == s.stepID && !s.fired {
		s.fired = true
		if _, err := s.RetryTasks(ctx, s.jobID, nil, now); err != nil {
			s.t.Errorf("retry in hook: %v", err)
		}
	}
	return s.Store.CancelPendingStep(ctx, id, reason, now)
}

// TestRetryBeforeTheCascadeKeepsTheDownstreamStep pins the stale-read
// cascade: CancelDependents decides from a step list read before the RetryJob
// lands. A cancel that guarded only that the downstream step was still pending
// would cancel it, and the retried upstream would then run, succeed, and the
// job would still end canceled. The cancel re-checks the upstream inside its
// own write, so the downstream step stays pending and runs once the upstream
// completes.
func TestRetryBeforeTheCascadeKeepsTheDownstreamStep(t *testing.T) {
	for name, inner := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			g := seedDependentStepJob(t, inner)
			job, down := g.job, g.steps[1]
			task, downTask := g.tasks[0][0], g.tasks[1][0]
			attempt := g.attempts[task.ID]
			st := &retryBeforeCascadeStore{Store: inner, jobID: job.ID, stepID: down.ID, t: t}
			s := newStatusTestScheduler(st)
			s.ctx = t.Context()

			msg := terminalReport(t, task, attempt, "failed", "boom")
			s.handleTaskStatusMessage(msg)
			if !msg.acked || msg.nacked {
				t.Fatalf("failure report must be acked (acked=%v nacked=%v)", msg.acked, msg.nacked)
			}
			if !st.fired {
				t.Fatal("the cascade never tried to cancel the downstream step")
			}
			if got := mustStep(t, inner, down.ID).Status; got != store.StepStatusPending {
				t.Fatalf("downstream step = %q, want pending (its upstream was retried)", got)
			}
			if got := mustTaskOf(t, inner, downTask.ID); got.Status != store.TaskStatusPending || got.FailureReason != "" {
				t.Fatalf("downstream task = %q/%q, want pending with no reason", got.Status, got.FailureReason)
			}
			if got := mustJob(t, inner, job.ID).Status; got.IsTerminal() {
				t.Fatalf("job = %q, want it still open (the retried upstream will run)", got)
			}
		})
	}
}
