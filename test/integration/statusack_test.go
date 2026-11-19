// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// statusack_test.go — a worker's first "running" and terminal statuses reach
// the server across a server restart and ahead of the worker's deregister,
// so finished work is never handed out again. The mock worker sends them
// through the real status publisher (internal/worker/status), which retries a
// report until SQI_TASK has stored it, and lists every attempt with a report
// still pending in its heartbeats, as a real worker does.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
)

// statusAckTasks is how many tasks TestStatusAck_TerminalsDuringServerStop
// runs at once.
const statusAckTasks = 20

// statusAckJobYAML is a job of statusAckTasks one-core tasks, so a worker
// advertising statusAckTasks cores leases them all at once.
var statusAckJobYAML = fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
name: Status Ack Test
steps:
  - name: Render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-%d"
    hostRequirements:
      amounts:
        - name: amount.worker.vcpu
          min: 1
    script:
      actions:
        onRun:
          command: echo
          args:
            - "{{Task.Param.Frame}}"
`, statusAckTasks)

// statusAckAttempt is the subset of one item of GET /api/v1/tasks/{id}/attempts
// these tests read.
type statusAckAttempt struct {
	AttemptNumber int        `json:"attempt_number"`
	Status        string     `json:"status"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at"`
}

// statusAckWorker is a mock worker that reports through a [status.Publisher]
// and heartbeats the attempts it holds: those it has leased and not yet
// reported terminal, plus those with a report not yet stored.
type statusAckWorker struct {
	*mockWorker

	pub *status.Publisher

	mu      sync.Mutex
	running map[string]bool // attempt ID → leased, no terminal reported yet
}

// newStatusAckWorker registers a worker advertising cpus cores on ts, with a
// connection that keeps reconnecting while the server restarts, and starts
// its heartbeat.
func newStatusAckWorker(t *testing.T, ts *testServer, farmID, queueID string, cpus int) *statusAckWorker {
	t.Helper()
	mw := newMockWorker(t, "nats://"+ts.NATSAddr, "worker-statusack-001", farmID, queueID,
		nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	w := &statusAckWorker{mockWorker: mw, pub: mw.publisher(), running: map[string]bool{}}
	w.registerCPUs(cpus)
	pollWorkerOnline(t, ts, w.id, 10*time.Second)
	w.heartbeat(time.Second)
	return w
}

// held returns the attempts this worker reports in its heartbeats.
func (w *statusAckWorker) held() []string {
	w.mu.Lock()
	ids := make([]string, 0, len(w.running))
	for id := range w.running {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	ids = append(ids, w.pub.PendingAttemptIDs()...)
	slices.Sort(ids)
	return slices.Compact(ids)
}

// heartbeat publishes a heartbeat listing held() every interval until the
// test ends. Unlike [mockWorker.startHeartbeatHolding] it keeps going after a
// failed publish, so heartbeats resume once a restarted server is back.
func (w *statusAckWorker) heartbeat(interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	w.t.Cleanup(cancel)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			data, err := json.Marshal(protocol.HeartbeatMsg{
				Version:          protocol.ProtocolVersion,
				Type:             protocol.TypeHeartbeat,
				WorkerID:         w.id,
				At:               time.Now().UTC(),
				AttemptsReported: true,
				ActiveAttemptIDs: w.held(),
			})
			if err != nil {
				return
			}
			pubCtx, pubCancel := context.WithTimeout(ctx, 2*time.Second)
			_, _ = w.js.Publish(pubCtx, bus.WorkerHeartbeatSubject(w.id), data) //nolint:errcheck // a heartbeat lost while the server is down is replaced by the next
			pubCancel()
		}
	}()
}

// lease pulls assignments until it holds n, and records them as running.
func (w *statusAckWorker) lease(n int) []protocol.AssignMsg {
	w.t.Helper()
	var got []protocol.AssignMsg
	deadline := time.Now().Add(30 * time.Second)
	for len(got) < n {
		left := time.Until(deadline)
		if left <= 0 {
			w.t.Fatalf("leased %d of %d tasks within 30s", len(got), n)
		}
		got = append(got, w.pullAssignments(left)...)
	}
	if len(got) != n {
		w.t.Fatalf("leased %d tasks, want %d", len(got), n)
	}
	w.mu.Lock()
	for _, a := range got {
		w.running[a.AttemptID] = true
	}
	w.mu.Unlock()
	return got
}

// reportRunning sends a's first "running".
func (w *statusAckWorker) reportRunning(a protocol.AssignMsg) {
	w.pub.Running(&a, "test-session-"+a.TaskID, time.Now())
}

// reportSucceeded sends a's terminal "succeeded", ending now.
func (w *statusAckWorker) reportSucceeded(a protocol.AssignMsg) {
	w.mu.Lock()
	delete(w.running, a.AttemptID)
	w.mu.Unlock()
	exit := 0
	w.pub.Terminal(&a, "test-session-"+a.TaskID, "succeeded", &exit, "", nil, time.Now())
}

// flush waits until every report has been stored, failing the test if one
// has not within timeout.
func (w *statusAckWorker) flush(timeout time.Duration) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if dropped := w.pub.Flush(ctx); dropped != 0 {
		w.t.Fatalf("%d status reports not stored within %s", dropped, timeout)
	}
}

// requirePending fails the test unless attemptID has a report not yet stored.
func (w *statusAckWorker) requirePending(attemptID string) {
	w.t.Helper()
	if !slices.Contains(w.pub.PendingAttemptIDs(), attemptID) {
		w.t.Fatalf("attempt %s has no pending report; want its first try to have failed with the server down", attemptID)
	}
}

// getStatusAckAttempts fetches a task's attempts.
func getStatusAckAttempts(t *testing.T, ts *testServer, taskID string) []statusAckAttempt {
	t.Helper()
	var resp struct {
		Items []statusAckAttempt `json:"items"`
	}
	mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID+"/attempts"), nil, "", http.StatusOK, &resp)
	return resp.Items
}

// waitTaskStatus polls a task until it reaches want, failing after timeout.
func waitTaskStatus(t *testing.T, ts *testServer, taskID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := getTaskDetail(t, ts, taskID).Status
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s after %s: status %q, want %q", taskID, timeout, got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireOneSucceededAttempt fails the test unless the task succeeded on its
// one attempt, which ended no earlier than it started and no later than now.
func requireOneSucceededAttempt(t *testing.T, ts *testServer, taskID string) {
	t.Helper()
	if got := getTaskDetail(t, ts, taskID).Status; got != "succeeded" {
		t.Errorf("task %s: status %q, want succeeded", taskID, got)
	}
	attempts := getStatusAckAttempts(t, ts, taskID)
	now := time.Now()
	if len(attempts) != 1 {
		t.Errorf("task %s: %d attempts %+v, want exactly 1 (the task was run again)", taskID, len(attempts), attempts)
		return
	}
	a := attempts[0]
	if a.Status != "succeeded" {
		t.Errorf("task %s attempt %d: status %q, want succeeded", taskID, a.AttemptNumber, a.Status)
	}
	switch {
	case a.EndedAt == nil:
		t.Errorf("task %s attempt %d: no ended_at", taskID, a.AttemptNumber)
	case a.EndedAt.Before(a.StartedAt) || a.EndedAt.After(now):
		t.Errorf("task %s attempt %d: started_at %s, ended_at %s, now %s: want started_at <= ended_at <= now",
			taskID, a.AttemptNumber, a.StartedAt.Format(time.RFC3339Nano), a.EndedAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestStatusAck_TerminalSurvivesServerRestart sends a terminal status while
// the server is down. Its first try fails, the publisher retries it, and once
// the server is back the task succeeds on its one attempt.
func TestStatusAck_TerminalSurvivesServerRestart(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	w := newStatusAckWorker(t, ts, farmID, queueID, 4)

	jobID := submitJob(t, ts, farmID, queueID)
	a := w.lease(1)[0]
	w.reportRunning(a)
	w.flush(10 * time.Second)
	waitTaskStatus(t, ts, a.TaskID, "running", 10*time.Second)

	ts.Stop(t)
	w.reportSucceeded(a)
	w.requirePending(a.AttemptID)

	ts = restartServer(t, ts)
	if final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 60*time.Second); final != "completed" {
		t.Fatalf("job final status: got %q, want completed", final)
	}
	requireOneSucceededAttempt(t, ts, a.TaskID)
}

// TestStatusAck_RunningSurvivesServerRestart sends an attempt's first
// "running" while the server is down; once the server is back the task is
// running on its one attempt.
func TestStatusAck_RunningSurvivesServerRestart(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	w := newStatusAckWorker(t, ts, farmID, queueID, 4)

	submitJob(t, ts, farmID, queueID)
	a := w.lease(1)[0]

	ts.Stop(t)
	w.reportRunning(a)
	w.requirePending(a.AttemptID)

	ts = restartServer(t, ts)
	waitTaskStatus(t, ts, a.TaskID, "running", 30*time.Second)
	w.flush(10 * time.Second)
	attempts := getStatusAckAttempts(t, ts, a.TaskID)
	if len(attempts) != 1 || attempts[0].Status != "running" {
		t.Fatalf("task %s attempts: %+v, want exactly one, running", a.TaskID, attempts)
	}
}

// TestStatusAck_TerminalsDuringServerStop finishes statusAckTasks running
// tasks while the server shuts down: some terminals are stored and applied
// before the stop, some stored but not yet applied, some sent while the
// server is down. After the restart every task has succeeded on its one
// attempt; none is run again.
func TestStatusAck_TerminalsDuringServerStop(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	w := newStatusAckWorker(t, ts, farmID, queueID, statusAckTasks)

	jobID := submitJobCustomYAML(t, ts, farmID, queueID, statusAckJobYAML)
	assigns := w.lease(statusAckTasks)
	for _, a := range assigns {
		w.reportRunning(a)
	}
	w.flush(10 * time.Second)
	for _, a := range assigns {
		waitTaskStatus(t, ts, a.TaskID, "running", 10*time.Second)
	}

	// Each terminal starts 10 ms after the one before it, on its own goroutine,
	// so one whose first try waits out the ack timeout while the broker is down
	// does not hold back the rest.
	var sends sync.WaitGroup
	sends.Go(func() {
		for i, a := range assigns {
			if i > 0 {
				time.Sleep(10 * time.Millisecond)
			}
			sends.Go(func() { w.reportSucceeded(a) })
		}
	})
	ts.Stop(t)
	t.Logf("server stopped with %d of %d attempts holding an unstored report", len(w.pub.PendingAttemptIDs()), statusAckTasks)
	ts = restartServer(t, ts)
	sends.Wait()

	if final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 60*time.Second); final != "completed" {
		t.Fatalf("job final status: got %q, want completed", final)
	}
	ids := jobTaskIDs(t, ts, jobID)
	if len(ids) != statusAckTasks {
		t.Fatalf("job has %d tasks, want %d", len(ids), statusAckTasks)
	}
	for _, id := range ids {
		requireOneSucceededAttempt(t, ts, id)
	}
}

// TestStatusAck_DeregisterRightAfterTerminal deregisters the worker as soon
// as its terminal status is stored. The server applies that status before it
// takes the worker offline and reclaims what it held, so the task stays
// succeeded on its one attempt.
func TestStatusAck_DeregisterRightAfterTerminal(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	w := newStatusAckWorker(t, ts, farmID, queueID, 4)

	jobID := submitJob(t, ts, farmID, queueID)
	a := w.lease(1)[0]
	w.reportSucceeded(a)
	w.flush(10 * time.Second)
	w.deregister()

	waitWorkerStatus(t, ts, w.id, "offline", 30*time.Second)
	if final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 30*time.Second); final != "completed" {
		t.Fatalf("job final status: got %q, want completed", final)
	}
	requireOneSucceededAttempt(t, ts, a.TaskID)
	if ts.Logs.has("scheduler: deregister applied before the task-status consumer caught up", nil) {
		t.Error("the deregister was applied before the task-status consumer caught up; want it deferred until then")
	}
}

// waitWorkerStatus polls a worker until it reaches want, failing after timeout.
func waitWorkerStatus(t *testing.T, ts *testServer, workerID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var got workerResp
		mustDoJSON(t, http.MethodGet, apiURL(ts, "/api/v1/workers/"+workerID), nil, "", http.StatusOK, &got)
		if got.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s after %s: status %q, want %q", workerID, timeout, got.Status, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
