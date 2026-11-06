// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

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

// TestCancelBeforeWorkerSubscribes drives the window between a lease and the
// worker subscribing to the task's cancel subject: the job is canceled after
// the worker has its assignment and before it subscribes, so the cancel the
// server publishes then reaches no one. The worker then subscribes and
// publishes "running", as sqi-worker does, and the server must answer that
// report for the attempt it has closed with a cancel naming the attempt.
func TestCancelBeforeWorkerSubscribes(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)

	const workerID = "worker-cancel-window"
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
	h := cancel.New(worker.nc, calls, slog.New(slog.DiscardHandler))
	if err := h.Register(assign.TaskID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { h.Deregister(assign.TaskID) })
	worker.publishStatus(assign, "running", nil)

	select {
	case got := <-calls:
		if got.taskID != assign.TaskID || got.attemptID != assign.AttemptID {
			t.Fatalf("cancel = task %s attempt %s, want task %s attempt %s",
				got.taskID, got.attemptID, assign.TaskID, assign.AttemptID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no cancel reached the worker after its running report for the canceled attempt")
	}
}
