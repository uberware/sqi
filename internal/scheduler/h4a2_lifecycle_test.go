// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
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
