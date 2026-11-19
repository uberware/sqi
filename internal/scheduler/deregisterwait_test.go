// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for deferring a deregister's reclaim until the task-status consumer
// has applied SQI_TASK up to the deregister's broker stamp.

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
)

const deregisterWarn = "scheduler: deregister applied before the task-status consumer caught up"

// deregisterWaitFixture is a membership fixture with one registered worker
// w1 (instance inst-1) holding the running task t1, a scheduler clock the
// test sets, and trackers that have processed no marker.
type deregisterWaitFixture struct {
	membershipFixture

	clock   *testClock
	attempt store.TaskAttempt
	// stamp is the deregister message's broker stamp.
	stamp time.Time
}

func newDeregisterWaitFixture(t *testing.T) deregisterWaitFixture {
	t.Helper()
	f := newMembershipFixture(t, false)
	f.register(t, "w1", "inst-1", "")
	_, attempts := storetest.NewJob("j1", capFarm, capQueue).
		Step("s1", store.StepStatusReady).
		Task("t1", "s1", store.TaskStatusRunning).
		SubmitLeasing(t, f.st, "w1")
	stamp := time.Now().UTC()
	clock := &testClock{t: stamp.Add(time.Second)}
	f.s.now = clock.now
	noIngestMarker(f.s)
	return deregisterWaitFixture{membershipFixture: f, clock: clock, attempt: attempts["t1"], stamp: stamp}
}

// deregisterMsg is w1's deregister for instance, stamped by the broker at
// the fixture's stamp.
func (f deregisterWaitFixture) deregisterMsg(t *testing.T, instance string) *fakeJSMsg {
	t.Helper()
	return &fakeJSMsg{
		subject: bus.WorkerDeregisterSubject("w1"),
		stamp:   f.stamp,
		data: workerMsgJSON(t, protocol.DeregisterMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeDeregister,
			WorkerID: "w1", InstanceID: instance,
		}),
	}
}

// requireDeferred fails unless msg was handed back for a re-check in
// deregisterRecheckDelay, unacknowledged, with w1 online and t1 running.
func (f deregisterWaitFixture) requireDeferred(t *testing.T, msg *fakeJSMsg) {
	t.Helper()
	if msg.acked {
		t.Fatal("deferred deregister was acknowledged")
	}
	if !msg.nacked || msg.nakDelay != deregisterRecheckDelay {
		t.Fatalf("deregister nak = %v delay %v, want a nak with delay %v", msg.nacked, msg.nakDelay, deregisterRecheckDelay)
	}
	if w := f.worker(t, "w1"); w.Status != store.WorkerStatusOnline {
		t.Fatalf("w1 status = %q while the deregister is deferred, want online", w.Status)
	}
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusRunning {
		t.Fatalf("t1 = %s while the deregister is deferred, want running", got)
	}
	f.requireAttemptOpen(t)
	// The deferral is not an ingest failure: nothing waits in the worker
	// tracker to be applied again.
	if n := f.s.ingestWorker.pendingCount(f.clock.t); n != 0 {
		t.Fatalf("worker tracker holds %d pending messages, want 0", n)
	}
}

func (f deregisterWaitFixture) requireAttemptOpen(t *testing.T) {
	t.Helper()
	a, err := f.st.GetTaskAttempt(t.Context(), f.attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if a.EndedAt != nil {
		t.Fatalf("attempt ended at %v, want it open", a.EndedAt)
	}
}

// requireApplied fails unless msg was acknowledged without a nak and w1 is
// offline.
func (f deregisterWaitFixture) requireApplied(t *testing.T, msg *fakeJSMsg) {
	t.Helper()
	if !msg.acked {
		t.Fatal("deregister was not acknowledged")
	}
	if msg.nacked {
		t.Fatalf("deregister was nak'd (delay %v), want it applied", msg.nakDelay)
	}
	if w := f.worker(t, "w1"); w.Status != store.WorkerStatusOffline {
		t.Fatalf("w1 status = %q, want offline", w.Status)
	}
}

// redeliver is msg delivered again, with its broker stamp unchanged.
func redeliver(msg *fakeJSMsg) *fakeJSMsg {
	return &fakeJSMsg{subject: msg.subject, data: msg.data, stamp: msg.stamp, metaErr: msg.metaErr}
}

func TestDeregisterWait_FitsWorkerConsumerDeliveries(t *testing.T) {
	if n := int(deregisterApplyWait / deregisterRecheckDelay); n >= bus.WorkerConsumerMaxDeliver-1 {
		t.Fatalf("a deferred deregister can be redelivered %d times, want fewer than %d (MaxDeliver %d less the final delivery)",
			n, bus.WorkerConsumerMaxDeliver-1, bus.WorkerConsumerMaxDeliver)
	}
}

func TestDeregister_DefersUntilTaskConsumerCatchesUp(t *testing.T) {
	f := newDeregisterWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))

	first := f.deregisterMsg(t, "inst-1")
	f.s.handleWorkerMessage(first)
	f.requireDeferred(t, first)

	f.s.ingestTask.marker(f.stamp.Add(500 * time.Millisecond))
	again := redeliver(first)
	f.s.handleWorkerMessage(again)
	f.requireApplied(t, again)
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusReady {
		t.Errorf("t1 = %s after the deregister, want ready (reclaimed)", got)
	}
	a, err := f.st.GetTaskAttempt(t.Context(), f.attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if a.EndedAt == nil {
		t.Error("attempt still open after the deregister was applied")
	}
}

func TestDeregister_StatusStoredBeforeDeregisterIsAppliedFirst(t *testing.T) {
	f := newDeregisterWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))

	first := f.deregisterMsg(t, "inst-1")
	f.s.handleWorkerMessage(first)
	f.requireDeferred(t, first)

	// The worker's succeeded report, stored on SQI_TASK before the
	// deregister, reaches the task-status consumer.
	exitCode := 0
	status := &fakeJSMsg{
		subject: bus.TaskStatusSubject("w1", "j1"),
		stamp:   f.stamp.Add(-200 * time.Millisecond),
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeTaskStatus,
			TaskID: "t1", AttemptID: f.attempt.ID, JobID: "j1", WorkerID: "w1",
			Status: "succeeded", ExitCode: &exitCode, At: f.stamp,
		}),
	}
	f.s.handleTaskStatusMessage(status)
	if !status.acked {
		t.Fatal("succeeded report was not acknowledged")
	}

	f.s.ingestTask.marker(f.stamp.Add(500 * time.Millisecond))
	again := redeliver(first)
	f.s.handleWorkerMessage(again)
	f.requireApplied(t, again)
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusSucceeded {
		t.Errorf("t1 = %s, want succeeded: the deregister must not reclaim finished work", got)
	}
	a, err := f.st.GetTaskAttempt(t.Context(), f.attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if a.Status != store.AttemptStatusSucceeded {
		t.Errorf("attempt status = %q, want succeeded", a.Status)
	}
}

func TestDeregister_AppliesAtOnceWhenCaughtUp(t *testing.T) {
	f := newDeregisterWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(time.Second))

	msg := f.deregisterMsg(t, "inst-1")
	f.s.handleWorkerMessage(msg)
	f.requireApplied(t, msg)
}

func TestDeregister_BeforeFirstMarkerDefers(t *testing.T) {
	f := newDeregisterWaitFixture(t)

	msg := f.deregisterMsg(t, "inst-1")
	f.s.handleWorkerMessage(msg)
	f.requireDeferred(t, msg)
}

func TestDeregister_ProceedsAfterApplyWait(t *testing.T) {
	f := newDeregisterWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))
	f.clock.t = f.stamp.Add(deregisterApplyWait)

	msg := f.deregisterMsg(t, "inst-1")
	f.s.handleWorkerMessage(msg)
	f.requireApplied(t, msg)
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusReady {
		t.Errorf("t1 = %s, want ready (reclaimed)", got)
	}
	if n := warnCount(f.logs.String(), deregisterWarn, "worker_id=w1"); n != 1 {
		t.Errorf("warnings %q naming w1 = %d, want 1:\n%s", deregisterWarn, n, f.logs.String())
	}
}

func TestDeregister_NoBrokerStampAppliesAtOnce(t *testing.T) {
	f := newDeregisterWaitFixture(t)

	msg := f.deregisterMsg(t, "inst-1")
	msg.metaErr = errors.New("no metadata")
	f.s.handleWorkerMessage(msg)
	f.requireApplied(t, msg)
}

// A deregister deferred while a new incarnation of the worker registers is
// still a no-op when it is finally applied: the instance-ID guard runs on
// every delivery.
func TestDeregister_DeferredFromSupersededInstanceLeavesWorkerOnline(t *testing.T) {
	f := newDeregisterWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))

	first := f.deregisterMsg(t, "inst-1")
	f.s.handleWorkerMessage(first)
	f.requireDeferred(t, first)

	f.register(t, "w1", "inst-2", "")
	f.s.ingestTask.marker(f.stamp.Add(500 * time.Millisecond))
	again := redeliver(first)
	f.s.handleWorkerMessage(again)
	if !again.acked || again.nacked {
		t.Fatalf("superseded deregister acked %v nak'd %v, want acked only", again.acked, again.nacked)
	}
	if w := f.worker(t, "w1"); w.Status != store.WorkerStatusOnline {
		t.Errorf("w1 status = %q, want online: the deregister came from a replaced process", w.Status)
	}
}
