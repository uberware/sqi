// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
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
	if err := forceAssign(st, task.ID, statusTestWorkerID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}
	s.cfg.AssignedTaskTimeout = time.Minute
	s.reapStaleAssignedTasks(t.Context())
	res, err := st.LeaseTask(t.Context(), store.LeaseRequest{
		TaskID: task.ID, WorkerID: "w-new", AttemptID: uuid.NewString(), Now: time.Now().UTC(),
	})
	if err != nil || res.Outcome != store.LeaseLeased {
		t.Fatalf("re-lease = (%+v, %v), want leased", res, err)
	}
	return res.Attempt
}

// TestH4a2_SupersededRunningReportIsIgnored pins item 9's first late-report
// hole: a superseded attempt's "running" report no longer moves the new
// lease's task to running, and emits no task event.
func TestH4a2_SupersededRunningReportIsIgnored(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, _, task, stale := seedStatusFixture(t, st, store.TaskStatusAssigned)
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

// TestH4a2_CancelEchoAfterRetryLeavesTaskReady: the user cancels a running
// task, retries it, then the old worker's "canceled" echo arrives. The task
// must stay ready (v0.3.0 and H4a re-canceled it).
func TestH4a2_CancelEchoAfterRetryLeavesTaskReady(t *testing.T) {
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

// TestH4a2_RequeueAfterReleaseLeavesTheNewLease pins item 9's third
// late-report hole: an offline reclaim and a new lease landing between
// RecordTaskFailure and the requeue must not return the new lease to ready.
func TestH4a2_RequeueAfterReleaseLeavesTheNewLease(t *testing.T) {
	for name, base := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, _, task, attempt := seedStatusFixture(t, base, store.TaskStatusAssigned)
			now := time.Now().UTC()
			// The offline reclaim matches on assigned_worker_id, which the
			// status fixture leaves empty.
			if err := forceAssign(base, task.ID, statusTestWorkerID, now); err != nil {
				t.Fatalf("AssignTask: %v", err)
			}
			if _, err := base.RegisterWorker(t.Context(), store.Worker{
				ID: statusTestWorkerID, FarmID: "farm-1", Hostname: "h", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
			}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}
			var fresh store.TaskAttempt
			st := &reclaimBeforeRequeueStore{Store: base}
			st.hook = &once{fn: func() {
				if _, err := base.OfflineWorker(context.Background(), statusTestWorkerID, time.Now().UTC()); err != nil {
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
			if v := claimViolations(t, base); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}
