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
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/status"
)

// statusAckTasks is how many tasks TestStatusAck_TerminalsDuringServerStop
// runs at once.
const statusAckTasks = 20

// statusAckJobYAML returns a job of n one-core tasks, so a worker advertising
// n cores leases them all at once.
func statusAckJobYAML(n int) string {
	return fmt.Sprintf(`specificationVersion: "jobtemplate-2023-09"
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
`, n)
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
	w.startHeartbeatHolding(time.Second, w.held)
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
	return w.pub.WithPending(ids)
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

// endedJitter is how far past the instant a terminal was sent its stored
// ended_at may fall: the server places the end at the broker's stamp minus
// the age the worker measured, and the two clocks are read a little apart.
const endedJitter = time.Second

// requireOneSucceededAttempt fails the test unless the task succeeded on its
// one attempt, which ended no earlier than it started and no later than now.
// A non-zero endedBy is a further bound: the terminal was sent before it, so
// ended_at must not be later than endedBy plus endedJitter however long the
// report then took to be stored.
func requireOneSucceededAttempt(t *testing.T, ts *testServer, taskID string, endedBy time.Time) {
	t.Helper()
	if got := getTaskDetail(t, ts, taskID).Status; got != "succeeded" {
		t.Errorf("task %s: status %q, want succeeded", taskID, got)
	}
	attempts := getTaskAttempts(t, ts, taskID).Items
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
	case !endedBy.IsZero() && a.EndedAt.After(endedBy.Add(endedJitter)):
		t.Errorf("task %s attempt %d: ended_at %s, more than %s after its terminal was sent (by %s): the retry delay was counted as run time",
			taskID, a.AttemptNumber, a.EndedAt.Format(time.RFC3339Nano), endedJitter, endedBy.Format(time.RFC3339Nano))
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestStatusAck_TerminalSurvivesServerRestart sends a terminal status while
// the server is down. Its first try fails, the publisher retries it, and once
// the server is back the task succeeds on its one attempt. The server stays
// down for more than endedJitter after the terminal is sent, so an ended_at
// that counted the retry delay as run time would fall past the bound.
func TestStatusAck_TerminalSurvivesServerRestart(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	w := newStatusAckWorker(t, ts, farmID, queueID, 4)

	jobID := submitJob(t, ts, farmID, queueID)
	a := w.lease(1)[0]
	w.reportRunning(a)
	w.flush(10 * time.Second)
	pollTaskStatus(t, ts, a.TaskID, []string{"running"}, 10*time.Second)

	ts.Stop(t)
	w.reportSucceeded(a) // the task ends now, with the server down
	sentBy := time.Now()
	w.requirePending(a.AttemptID)

	time.Sleep(2 * time.Second)
	ts = restartServer(t, ts)
	if final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 60*time.Second); final != "completed" {
		t.Fatalf("job final status: got %q, want completed", final)
	}
	requireOneSucceededAttempt(t, ts, a.TaskID, sentBy)
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
	pollTaskStatus(t, ts, a.TaskID, []string{"running"}, 30*time.Second)
	w.flush(10 * time.Second)
	attempts := getTaskAttempts(t, ts, a.TaskID).Items
	if len(attempts) != 1 || attempts[0].Status != "running" {
		t.Fatalf("task %s attempts: %+v, want exactly one, running", a.TaskID, attempts)
	}
}

// TestStatusAck_TerminalsDuringServerStop finishes statusAckTasks running
// tasks while the server shuts down: some terminals are stored and applied
// before the stop, some stored but not yet applied, and usually some sent
// while the server is down (how many depends on how long the stop takes, and
// is logged). After the restart every task has succeeded on its one attempt;
// none is run again.
func TestStatusAck_TerminalsDuringServerStop(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	w := newStatusAckWorker(t, ts, farmID, queueID, statusAckTasks)

	jobID := submitJobCustomYAML(t, ts, farmID, queueID, statusAckJobYAML(statusAckTasks))
	assigns := w.lease(statusAckTasks)
	for _, a := range assigns {
		w.reportRunning(a)
	}
	w.flush(10 * time.Second)
	for _, a := range assigns {
		pollTaskStatus(t, ts, a.TaskID, []string{"running"}, 10*time.Second)
	}

	// Each terminal starts 10 ms after the one before it, on its own goroutine,
	// so one whose first try waits out the ack timeout while the broker is down
	// does not hold back the rest.
	var sends sync.WaitGroup
	dispatched := make(chan struct{})
	sends.Go(func() {
		defer close(dispatched)
		for i, a := range assigns {
			if i > 0 {
				time.Sleep(10 * time.Millisecond)
			}
			sends.Go(func() { w.reportSucceeded(a) })
		}
	})
	ts.Stop(t)
	unstored := len(w.pub.PendingAttemptIDs())
	t.Logf("server stopped with %d of %d attempts holding an unstored report", unstored, statusAckTasks)
	if unstored == 0 {
		// The split depends on how long the stop takes; TestStatusAck_TerminalSurvivesServerRestart
		// covers a terminal sent while the server is down on every run.
		t.Log("every terminal was stored before the server stopped; this run covered no terminal in flight across the stop")
	}
	<-dispatched // every task has ended before the restart begins
	restartAt := time.Now()
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
		requireOneSucceededAttempt(t, ts, id, restartAt)
	}
}

// TestStatusAck_DeregisterRightAfterTerminal deregisters the worker as soon
// as its terminal status is stored. The server applies that status before it
// takes the worker offline and reclaims what it held, so the task stays
// succeeded on its one attempt.
//
// This covers the end-to-end outcome only: it cannot tell a deferred
// deregister from one that happened to arrive after the status was applied.
// The deferral itself is pinned by the scheduler unit tests
// TestDeregister_StatusStoredBeforeDeregisterIsAppliedFirst and
// TestDeregister_DefersUntilTaskConsumerCatchesUp.
func TestStatusAck_DeregisterRightAfterTerminal(t *testing.T) {
	ts := startServer(t)
	farmID, queueID := seedFarmAndQueue(t, ts)
	w := newStatusAckWorker(t, ts, farmID, queueID, 4)

	jobID := submitJob(t, ts, farmID, queueID)
	a := w.lease(1)[0]
	w.reportSucceeded(a)
	w.flush(10 * time.Second)
	w.deregister()

	pollWorkerOffline(t, ts, w.id, 30*time.Second)
	if final := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, 30*time.Second); final != "completed" {
		t.Fatalf("job final status: got %q, want completed", final)
	}
	requireOneSucceededAttempt(t, ts, a.TaskID, time.Time{})
	if ts.Logs.has("scheduler: deregister applied before the task-status consumer caught up", nil) {
		t.Error("the deregister was applied before the task-status consumer caught up; want it deferred until then")
	}
}

// Reaper timing for TestStatusAck_RunningRetriedAcrossStopIsNotReaped.
const (
	// reapTimeout is the server's AssignedTaskTimeout: a task assigned for
	// longer than this with no running applied is returned to the queue.
	reapTimeout = 3 * time.Second
	// reapInterval is the server's HeartbeatSweepInterval, which the reaper
	// runs on.
	reapInterval = 500 * time.Millisecond
	// reapGrace is the server's BusReconnectGrace: for this long after a
	// (re)start the reaper reclaims nothing. The server's default is minutes;
	// this is the shortest that still covers the publisher's retry backoff
	// (up to its 5 s RetryMax plus a 2 s ack timeout), as the default does.
	reapGrace = 15 * time.Second
)

// TestStatusAck_RunningRetriedAcrossStopIsNotReaped leases two tasks and
// stops the server. The worker then sends one task's first "running", whose
// first try fails, and the server stays down for longer than
// AssignedTaskTimeout. After the restart the retried report lands before the
// reaper may act, so that task is running on its one attempt and stays so
// through further reaper sweeps. The other task, never reported running, is
// reaped: proof the reaper ran over the same window.
func TestStatusAck_RunningRetriedAcrossStopIsNotReaped(t *testing.T) {
	ts := startServerWith(t, func(c *server.Config) {
		c.Scheduler.AssignedTaskTimeout = reapTimeout
		c.Scheduler.HeartbeatSweepInterval = reapInterval
		c.Scheduler.BusReconnectGrace = reapGrace
	})
	farmID, queueID := seedFarmAndQueue(t, ts)
	w := newStatusAckWorker(t, ts, farmID, queueID, 4)

	submitJobCustomYAML(t, ts, farmID, queueID, statusAckJobYAML(2))
	assigns := w.lease(2)
	task, control := assigns[0], assigns[1]

	ts.Stop(t)
	w.reportRunning(task)
	w.requirePending(task.AttemptID)
	time.Sleep(reapTimeout + time.Second) // the server is down: nothing to poll
	ts = restartServer(t, ts)
	restartAt := time.Now()

	pollTaskStatus(t, ts, task.TaskID, []string{"running"}, reapGrace)
	w.flush(10 * time.Second)

	// The reaper may act from reapGrace after the restart. Watch the task
	// until the control has been reaped and a further timeout and sweep have
	// passed, failing as soon as it leaves running.
	controlReaped := false
	watchUntil := restartAt.Add(reapGrace + reapTimeout + 2*reapInterval)
	for {
		if got := getTaskDetail(t, ts, task.TaskID).Status; got != "running" {
			t.Fatalf("task %s: status %q %s after the restart, want running (it was reaped)", task.TaskID, got, time.Since(restartAt).Round(time.Millisecond))
		}
		if !controlReaped && getTaskDetail(t, ts, control.TaskID).Status == "ready" {
			controlReaped = true
			if later := time.Now().Add(reapTimeout + 2*reapInterval); later.After(watchUntil) {
				watchUntil = later
			}
		}
		if controlReaped && time.Now().After(watchUntil) {
			break
		}
		if time.Since(restartAt) > reapGrace+30*time.Second {
			t.Fatalf("control task %s never reaped (status %q): the reaper did not run", control.TaskID, getTaskDetail(t, ts, control.TaskID).Status)
		}
		time.Sleep(200 * time.Millisecond)
	}

	attempts := getTaskAttempts(t, ts, task.TaskID).Items
	if len(attempts) != 1 || attempts[0].Status != "running" {
		t.Fatalf("task %s attempts: %+v, want exactly one, running", task.TaskID, attempts)
	}
}
