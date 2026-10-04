// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
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
