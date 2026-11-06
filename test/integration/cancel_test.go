// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/worker/cancel"
)

// cancelCall is one TaskCanceler.Cancel call.
type cancelCall struct{ taskID, attemptID string }

// chanCanceler reports each Cancel call on a channel.
type chanCanceler chan cancelCall

func (c chanCanceler) Cancel(taskID, attemptID string) bool {
	c <- cancelCall{taskID, attemptID}
	return true
}

// subscribeCancels subscribes calls to worker's cancel subject, as sqi-worker
// does before it asks for work.
func subscribeCancels(t *testing.T, worker *mockWorker, calls chanCanceler) {
	t.Helper()
	sub, err := cancel.Subscribe(worker.nc, worker.id, calls, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("cancel.Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() }) //nolint:errcheck // the connection may already be closed
}

// awaitCancel waits for one cancel on calls and checks it names taskID and
// attemptID ("" for a task-wide cancel).
func awaitCancel(t *testing.T, calls chanCanceler, taskID, attemptID string, timeout time.Duration, why string) {
	t.Helper()
	select {
	case got := <-calls:
		if got.taskID != taskID || got.attemptID != attemptID {
			t.Fatalf("cancel = task %s attempt %q, want task %s attempt %q", got.taskID, got.attemptID, taskID, attemptID)
		}
	case <-time.After(timeout):
		t.Fatal(why)
	}
}

// TestCancelReachesTheWorkerRightAfterItsLease pins that a cancel committed
// straight after a lease reaches the worker that took it: the worker's cancel
// subscription exists before it asks for work, so there is no window between
// the lease and a subscription for a cancel to fall into.
func TestCancelReachesTheWorkerRightAfterItsLease(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	const workerID = "worker-cancel-window"
	worker := newMockWorker(t, "nats://"+ts.NATSAddr, workerID, farmID, queueID)
	worker.register()
	worker.startHeartbeat(5 * time.Second)
	calls := make(chanCanceler, 4)
	subscribeCancels(t, worker, calls)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	jobID := submitJob(t, ts, farmID, queueID)
	assign := worker.pullAssignment(15 * time.Second)
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/jobs/"+jobID+"/cancel"),
		nil, "", http.StatusNoContent, nil)

	awaitCancel(t, calls, assign.TaskID, "", 15*time.Second,
		"the job cancel never reached the worker that had just leased its task")
}

// TestCancelResentOnARunningReportForAClosedAttempt drives a worker that
// misses a cancel (it is not subscribed when the cancel goes out, as when it
// is disconnected) and then publishes "running" for the attempt the cancel
// closed: the server must answer that report with a cancel naming the attempt.
func TestCancelResentOnARunningReportForAClosedAttempt(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	const workerID = "worker-cancel-running"
	worker := newMockWorker(t, "nats://"+ts.NATSAddr, workerID, farmID, queueID)
	worker.register()
	worker.startHeartbeat(5 * time.Second)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	jobID := submitJob(t, ts, farmID, queueID)
	assign := worker.pullAssignment(15 * time.Second)
	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/jobs/"+jobID+"/cancel"),
		nil, "", http.StatusNoContent, nil)
	pollJobStatus(t, ts, jobID, []string{"canceled"}, 10*time.Second)

	calls := make(chanCanceler, 4)
	subscribeCancels(t, worker, calls)
	worker.publishStatus(assign, "running", nil)

	awaitCancel(t, calls, assign.TaskID, assign.AttemptID, 15*time.Second,
		"no cancel reached the worker after its running report for the canceled attempt")
}

// TestCancelResentToAWorkerHoldingAClosedAttempt drives a run that misses both
// the cancel and the resend on its "running" report: the worker is not
// subscribed when the job is canceled, and never publishes "running". Its
// heartbeats still list the attempt, and the server's heartbeat sweep must
// send it a cancel naming that attempt.
func TestCancelResentToAWorkerHoldingAClosedAttempt(t *testing.T) {
	ts := startServerWith(t, func(c *server.Config) {
		c.Scheduler.WorkerTimeout = 5 * time.Second
		c.Scheduler.ReconcileGrace = 5 * time.Second
		c.Scheduler.HeartbeatSweepInterval = 500 * time.Millisecond
	})
	farmID, queueID := seedFarmAndQueue(t, ts)

	const workerID = "worker-cancel-held"
	worker := newMockWorker(t, "nats://"+ts.NATSAddr, workerID, farmID, queueID)
	worker.register()
	var held atomic.Pointer[[]string]
	held.Store(&[]string{})
	worker.startHeartbeatHolding(500*time.Millisecond, func() []string { return *held.Load() })
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	jobID := submitJob(t, ts, farmID, queueID)
	assign := worker.pullAssignment(15 * time.Second)
	held.Store(&[]string{assign.AttemptID})

	mustDoJSONClient(t, clientFor(ts), http.MethodPost, apiURL(ts, "/api/v1/jobs/"+jobID+"/cancel"),
		nil, "", http.StatusNoContent, nil)
	pollJobStatus(t, ts, jobID, []string{"canceled"}, 10*time.Second)

	calls := make(chanCanceler, 64)
	subscribeCancels(t, worker, calls)

	awaitCancel(t, calls, assign.TaskID, assign.AttemptID, 30*time.Second,
		"no cancel reached a worker whose heartbeats still list the canceled attempt")
}
