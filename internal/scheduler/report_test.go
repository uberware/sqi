// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// runningReport builds the "running" message a worker publishes for attempt.
func runningReport(t *testing.T, task store.Task, attempt store.TaskAttempt) *fakeJSMsg {
	t.Helper()
	return &fakeJSMsg{subject: statusTestSubject, data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
		Version: protocol.ProtocolVersion, TaskID: task.ID, AttemptID: attempt.ID,
		Status: "running", SessionID: "sess-old", At: time.Now().UTC(),
	})}
}

// releaseToNewWorker reaps the task's stale assignment and leases it to
// w-new, returning the new attempt.
func releaseToNewWorker(t *testing.T, st store.Store, s *Scheduler, task store.Task) store.TaskAttempt {
	t.Helper()
	s.cfg.AssignedTaskTimeout = time.Minute
	s.reapStaleAssignedTasks(t.Context())
	return storetest.Lease(t, st, store.LeaseRequest{TaskID: task.ID, WorkerID: "w-new"})
}

// TestSupersededRunningReportIsIgnored pins that a superseded attempt's
// "running" report does not move the new lease's task to running, and emits
// no task event.
func TestSupersededRunningReportIsIgnored(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, task, stale := seedStaleAssignment(t, st, statusTestWorkerID, nil)
			notifier := &recordingNotifier{}
			s := newStatusTestSchedulerWithNotifier(st, notifier)
			s.ctx = t.Context()
			fresh := releaseToNewWorker(t, st, s, task)
			notifier.tasks = nil

			msg := runningReport(t, task, stale)
			s.handleTaskStatusMessage(msg)

			if !msg.acked || msg.nacked {
				t.Fatalf("stale running report must be acked (acked=%v nacked=%v)", msg.acked, msg.nacked)
			}
			if got := mustTaskOf(t, st, task.ID); got.Status != store.TaskStatusAssigned || got.AssignedWorkerID != "w-new" {
				t.Fatalf("task = %q on %q, want assigned on w-new", got.Status, got.AssignedWorkerID)
			}
			if a := mustAttemptOf(t, st, fresh.ID); a.SessionID == "sess-old" {
				t.Fatal("the stale report's session ID landed on the new attempt")
			}
			if len(notifier.tasks) != 0 {
				t.Fatalf("task events = %+v, want none for a stale report", notifier.tasks)
			}
		})
	}
}

// TestCancelEchoAfterRetryLeavesTaskReady: the user cancels a running
// task, retries it, then the old worker's "canceled" echo arrives. The task
// must stay ready, not be re-canceled.
func TestCancelEchoAfterRetryLeavesTaskReady(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
			s := newTestScheduler(st, &stubBus{})
			s.ctx = t.Context()
			if err := s.CancelTask(t.Context(), task.ID); err != nil {
				t.Fatalf("CancelTask: %v", err)
			}
			if err := s.RetryTask(t.Context(), task.ID); err != nil {
				t.Fatalf("RetryTask: %v", err)
			}
			msg := terminalReport(t, task, attempt, "canceled", "")
			s.handleTaskStatusMessage(msg)
			if !msg.acked || msg.nacked {
				t.Fatalf("echo must be acked (acked=%v nacked=%v)", msg.acked, msg.nacked)
			}
			if got := mustTaskOf(t, st, task.ID).Status; got != store.TaskStatusReady {
				t.Fatalf("task = %q, want ready", got)
			}
		})
	}
}

// reclaimBeforeRequeueStore runs hook just before the failure fork's requeue:
// the window between RecordTaskFailure and RequeueTaskForRetry.
type reclaimBeforeRequeueStore struct {
	store.Store

	hook *once
}

func (s *reclaimBeforeRequeueStore) RequeueTaskForRetry(ctx context.Context, taskID, attemptID string, retryAfter, now time.Time) (bool, error) {
	s.hook.fire()
	return s.Store.RequeueTaskForRetry(ctx, taskID, attemptID, retryAfter, now)
}

// TestRequeueAfterReleaseLeavesTheNewLease pins that an offline reclaim and a
// new lease landing between RecordTaskFailure and the requeue do not return
// the new lease to ready.
func TestRequeueAfterReleaseLeavesTheNewLease(t *testing.T) {
	for name, base := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, _, task, attempt := seedStatusFixture(t, base, store.TaskStatusAssigned)
			now := time.Now().UTC()
			if _, _, err := base.RegisterWorker(t.Context(), store.Worker{
				ID: statusTestWorkerID, FarmID: "farm-1", Hostname: "h", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
			}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}
			var fresh store.TaskAttempt
			st := &reclaimBeforeRequeueStore{Store: base}
			st.hook = &once{fn: func() {
				if _, _, err := base.OfflineWorker(context.Background(), statusTestWorkerID, "", time.Now().UTC()); err != nil {
					t.Errorf("OfflineWorker: %v", err)
				}
				res, err := base.LeaseTask(context.Background(), store.LeaseRequest{
					TaskID: task.ID, WorkerID: "w-new", AttemptID: uuid.NewString(), Now: time.Now().UTC(),
				})
				if err != nil || res.Outcome != store.LeaseLeased {
					t.Errorf("re-lease = (%+v, %v)", res, err)
				}
				fresh = res.Attempt
			}}
			cfg := DefaultConfig()
			cfg.DefaultMaxAttempts, cfg.RetryDelay = 3, 0
			s := New(cfg, st, nil, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
			s.ctx = t.Context()

			s.handleTaskStatusMessage(terminalReport(t, task, attempt, "failed", "boom"))

			if got := mustTaskOf(t, base, task.ID); got.Status != store.TaskStatusAssigned || got.AssignedWorkerID != "w-new" {
				t.Fatalf("task = %q on %q, want assigned on w-new", got.Status, got.AssignedWorkerID)
			}
			if a := mustAttemptOf(t, base, fresh.ID); a.Status != store.AttemptStatusRunning {
				t.Fatalf("new attempt = %q, want running", a.Status)
			}
			if v := storetest.ClaimViolations(t, base); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// TestShutdownReportAndDeregisterAgreeInEitherOrder pins that a
// forced shutdown's "failed"/"worker_shutdown" report and the worker's
// deregister leave the same state whichever the server applies first, and
// neither counts a genuine failure.
func TestShutdownReportAndDeregisterAgreeInEitherOrder(t *testing.T) {
	for _, reportFirst := range []bool{true, false} {
		for name, st := range raceBackends(t) {
			t.Run(fmt.Sprintf("reportFirst=%v/%s", reportFirst, name), func(t *testing.T) {
				job, task, attempt, pool := seedClaimedFixture(t, st, store.TaskStatusAssigned)
				now := time.Now().UTC()
				if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
					ID: statusTestWorkerID, FarmID: "farm-1", Hostname: "h", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
				}); err != nil {
					t.Fatalf("RegisterWorker: %v", err)
				}
				cfg := DefaultConfig()
				cfg.DefaultMaxAttempts = 3
				s := New(cfg, st, nil, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
				s.ctx = t.Context()

				msg := terminalReport(t, task, attempt, "failed", protocol.MessageWorkerShutdown)
				report := func() { s.handleTaskStatusMessage(msg) }
				deregister := func() {
					if _, _, err := st.OfflineWorker(t.Context(), statusTestWorkerID, "", time.Now().UTC()); err != nil {
						t.Fatalf("OfflineWorker: %v", err)
					}
				}
				if reportFirst {
					report()
					deregister()
				} else {
					deregister()
					report()
				}

				if !msg.acked || msg.nacked {
					t.Fatalf("shutdown report must be acked (acked=%v nacked=%v)", msg.acked, msg.nacked)
				}
				got := mustTaskOf(t, st, task.ID)
				if got.Status != store.TaskStatusReady || got.FailedAttempts != 0 {
					t.Fatalf("task = %q failed_attempts=%d, want ready with 0", got.Status, got.FailedAttempts)
				}
				if j := mustJob(t, st, job.ID); j.FailedAttempts != 0 {
					t.Fatalf("job failed_attempts = %d, want 0", j.FailedAttempts)
				}
				if a := mustAttemptOf(t, st, attempt.ID); a.Status != store.AttemptStatusFailed {
					t.Fatalf("attempt = %q, want failed", a.Status)
				}
				if n := activeClaimsOf(t, st, pool.ID); n != 0 {
					t.Fatalf("active claims = %d, want 0", n)
				}
			})
		}
	}
}
