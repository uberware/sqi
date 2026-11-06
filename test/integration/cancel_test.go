// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/worker/cancel"
	"github.com/uberware/sqi/internal/worker/protocol"
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
	if err := h.Register(assign.TaskID, assign.AttemptID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { h.Deregister(assign.TaskID, assign.AttemptID) })
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

// TestCancelResentToAWorkerHoldingAClosedAttempt drives a run that misses both
// the cancel and the resend on its "running" report: the job is canceled
// before the worker subscribes, and the worker never publishes "running". Its
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
	h := cancel.New(worker.nc, calls, slog.New(slog.DiscardHandler))
	if err := h.Register(assign.TaskID, assign.AttemptID); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { h.Deregister(assign.TaskID, assign.AttemptID) })

	select {
	case got := <-calls:
		if got.taskID != assign.TaskID || got.attemptID != assign.AttemptID {
			t.Fatalf("cancel = task %s attempt %s, want task %s attempt %s",
				got.taskID, got.attemptID, assign.TaskID, assign.AttemptID)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no cancel reached a worker whose heartbeats still list the canceled attempt")
	}
}

// startHeartbeatHolding is [mockWorker.startHeartbeat] for a worker that
// reports the attempts it holds: each heartbeat lists attempts().
func (w *mockWorker) startHeartbeatHolding(interval time.Duration, attempts func() []string) {
	w.t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	w.t.Cleanup(stop)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				data, err := json.Marshal(protocol.HeartbeatMsg{
					Version: protocol.ProtocolVersion, Type: protocol.TypeHeartbeat,
					WorkerID: w.id, At: time.Now().UTC(),
					AttemptsReported: true, ActiveAttemptIDs: attempts(),
				})
				if err != nil {
					return
				}
				pubCtx, pubCancel := context.WithTimeout(ctx, 2*time.Second)
				_, pubErr := w.js.Publish(pubCtx, bus.WorkerHeartbeatSubject(w.id), data)
				pubCancel()
				if pubErr != nil {
					return
				}
			}
		}
	}()
}
