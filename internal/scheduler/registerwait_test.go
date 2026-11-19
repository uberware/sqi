// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for deferring a restarted worker's registration, whose write reclaims
// the previous process's attempts, until the task-status consumer has applied
// SQI_TASK up to the registration's broker stamp.

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const restartRegisterWarn = "scheduler: restart registration applied before the task-status consumer caught up"

// registerMsg is the first delivery of w1's registration for instance,
// stamped by the broker at the fixture's stamp.
func (f taskWaitFixture) registerMsg(t *testing.T, instance string) *fakeJSMsg {
	t.Helper()
	msg := registerMsgStamped(t, "w1", instance, f.stamp)
	msg.delivered = 1
	return msg
}

// requireRegisterDeferred is requireHeldBack for a registration, with w1
// still registered as inst-1.
func (f taskWaitFixture) requireRegisterDeferred(t *testing.T, msg *fakeJSMsg) {
	t.Helper()
	f.requireHeldBack(t, msg)
	if w := f.worker(t, "w1"); w.InstanceID != "inst-1" {
		t.Fatalf("w1 instance = %q while the registration is deferred, want inst-1", w.InstanceID)
	}
}

// requireRegistered fails unless msg was acknowledged without a nak and w1 is
// registered as instance.
func (f taskWaitFixture) requireRegistered(t *testing.T, msg *fakeJSMsg, instance string) {
	t.Helper()
	requireAckedOnly(t, msg)
	if w := f.worker(t, "w1"); w.InstanceID != instance {
		t.Fatalf("w1 instance = %q, want %q", w.InstanceID, instance)
	}
}

func TestRegister_RestartDefersUntilTaskConsumerCatchesUp(t *testing.T) {
	f := newTaskWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))

	first := f.registerMsg(t, "inst-2")
	f.s.handleWorkerMessage(first)
	f.requireRegisterDeferred(t, first)

	f.s.ingestTask.marker(f.stamp.Add(500 * time.Millisecond))
	again := redeliver(first)
	f.s.handleWorkerMessage(again)
	f.requireRegistered(t, again, "inst-2")
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusReady {
		t.Errorf("t1 = %s after the restart registration, want ready (reclaimed)", got)
	}
}

func TestRegister_RestartStatusStoredBeforeRegistrationIsAppliedFirst(t *testing.T) {
	f := newTaskWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))

	first := f.registerMsg(t, "inst-2")
	f.s.handleWorkerMessage(first)
	f.requireRegisterDeferred(t, first)

	// The previous process's succeeded report, stored on SQI_TASK before the
	// new process registered, reaches the task-status consumer.
	status := f.succeededMsg(t, 3, f.stamp.Add(-200*time.Millisecond))
	f.s.handleTaskStatusMessage(status)
	if !status.acked {
		t.Fatal("succeeded report was not acknowledged")
	}

	f.s.ingestTask.marker(f.stamp.Add(500 * time.Millisecond))
	again := redeliver(first)
	f.s.handleWorkerMessage(again)
	f.requireRegistered(t, again, "inst-2")
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusSucceeded {
		t.Errorf("t1 = %s, want succeeded: the restart registration must not reclaim finished work", got)
	}
	if a := mustAttemptOf(t, f.st, f.attempt.ID); a.Status != store.AttemptStatusSucceeded {
		t.Errorf("attempt status = %q, want succeeded", a.Status)
	}
}

func TestRegister_RestartAppliesAtOnceWhenCaughtUp(t *testing.T) {
	f := newTaskWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(time.Second))

	msg := f.registerMsg(t, "inst-2")
	f.s.handleWorkerMessage(msg)
	f.requireRegistered(t, msg, "inst-2")
}

// A reconnect re-registers the same process, which reclaims nothing, so it
// never waits.
func TestRegister_SameInstanceAppliesAtOnce(t *testing.T) {
	f := newTaskWaitFixture(t)

	msg := f.registerMsg(t, "inst-1")
	f.s.handleWorkerMessage(msg)
	f.requireRegistered(t, msg, "inst-1")
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusRunning {
		t.Errorf("t1 = %s after a same-instance registration, want running", got)
	}
}

// A restarted worker whose previous process holds no task has nothing to
// reclaim, so its registration does not wait for the task-status consumer.
func TestRegister_RestartWithNothingInFlightAppliesAtOnce(t *testing.T) {
	f := newTaskWaitFixture(t)
	status := f.succeededMsg(t, 3, f.stamp.Add(-200*time.Millisecond))
	f.s.handleTaskStatusMessage(status)
	if !status.acked {
		t.Fatal("succeeded report was not acknowledged")
	}

	msg := f.registerMsg(t, "inst-2")
	f.s.handleWorkerMessage(msg)
	f.requireRegistered(t, msg, "inst-2")
}

func TestRegister_FirstRegistrationAppliesAtOnce(t *testing.T) {
	f := newTaskWaitFixture(t)

	msg := registerMsgStamped(t, "w-new", "inst-x", f.stamp)
	f.s.handleWorkerMessage(msg)
	requireAckedOnly(t, msg)
}

// A registration the broker stamped before the one already recorded is
// discarded by the registration write, so it does not wait first.
func TestRegister_StaleRegistrationAppliesAtOnce(t *testing.T) {
	f := newTaskWaitFixture(t)
	current := registerMsgStamped(t, "w1", "inst-1", f.stamp.Add(time.Second))
	f.s.handleWorkerMessage(current)
	requireAckedOnly(t, current)

	stale := f.registerMsg(t, "inst-0")
	f.s.handleWorkerMessage(stale)
	f.requireRegistered(t, stale, "inst-1")
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusRunning {
		t.Errorf("t1 = %s after a stale registration, want running", got)
	}
}

// A registration carrying no instance ID keeps the stored one and reclaims
// nothing ([store.WorkerStore.RegisterWorker]), so it does not wait.
func TestRegister_NoInstanceIDAppliesAtOnce(t *testing.T) {
	f := newTaskWaitFixture(t)

	msg := f.registerMsg(t, "")
	f.s.handleWorkerMessage(msg)
	f.requireRegistered(t, msg, "inst-1")
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusRunning {
		t.Errorf("t1 = %s after a registration with no instance ID, want running", got)
	}
}

func TestRegister_RestartNoBrokerStampAppliesAtOnce(t *testing.T) {
	f := newTaskWaitFixture(t)

	msg := f.registerMsg(t, "inst-2")
	msg.metaErr = errors.New("no metadata")
	f.s.handleWorkerMessage(msg)
	f.requireRegistered(t, msg, "inst-2")
}

// A restart registration the task-status consumer never catches up to is
// deferred on every delivery before the taskWaitMaxRechecks-th and applied,
// with one warning, on that one.
func TestRegister_RestartAppliesAtMaxRechecks(t *testing.T) {
	f := newTaskWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))

	msg := f.registerMsg(t, "inst-2")
	msg.delivered = uint64(taskWaitMaxRechecks - 1)
	f.s.handleWorkerMessage(msg)
	f.requireRegisterDeferred(t, msg)
	if n := warnCount(f.logs.String(), restartRegisterWarn); n != 0 {
		t.Fatalf("warnings %q before the last re-check = %d, want 0:\n%s", restartRegisterWarn, n, f.logs.String())
	}

	last := redeliver(msg)
	f.s.handleWorkerMessage(last)
	f.requireRegistered(t, last, "inst-2")
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusReady {
		t.Errorf("t1 = %s, want ready (reclaimed)", got)
	}
	want := fmt.Sprintf("deliveries=%d", taskWaitMaxRechecks)
	if n := warnCount(f.logs.String(), restartRegisterWarn, "worker_id=w1", want); n != 1 {
		t.Errorf("warnings %q naming w1 and %s = %d, want 1:\n%s", restartRegisterWarn, want, n, f.logs.String())
	}
}

// A terminal status of the previous process stored before the registration,
// whose first apply failed, holds the registration back even once markers
// have moved the task tracker past its stamp.
func TestRegister_RestartDefersWhileWorkerStatusAwaitsRedelivery(t *testing.T) {
	f := newTaskWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))

	status := f.succeededMsg(t, 7, f.stamp.Add(-200*time.Millisecond))
	f.failFirstApply(t, status)

	f.s.ingestTask.marker(f.stamp.Add(500 * time.Millisecond))
	first := f.registerMsg(t, "inst-2")
	f.s.handleWorkerMessage(first)
	f.requireRegisterDeferred(t, first)

	again := redeliver(status)
	f.s.handleTaskStatusMessage(again)
	if !again.acked {
		t.Fatal("redelivered status was not acknowledged")
	}

	next := redeliver(first)
	f.s.handleWorkerMessage(next)
	f.requireRegistered(t, next, "inst-2")
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusSucceeded {
		t.Errorf("t1 = %s, want succeeded: the restart registration must not reclaim finished work", got)
	}
}

// A scheduler deposed while a restart registration would wait leaves the
// message alone (neither acked nor nak'd) for the new leader.
func TestRegister_DeposedWhileDeferringLeavesMessageAndAttempt(t *testing.T) {
	f := newTaskWaitFixture(t)
	f.s.ingestTask.marker(f.stamp.Add(-time.Second))
	f.s.fence.deposed.Store(true)

	msg := f.registerMsg(t, "inst-2")
	f.s.handleWorkerMessage(msg)
	if msg.acked || msg.nacked {
		t.Fatalf("deposed scheduler acked %v nak'd %v the registration, want neither", msg.acked, msg.nacked)
	}
	if got := mustTaskOf(t, f.st, "t1").Status; got != store.TaskStatusRunning {
		t.Errorf("t1 = %s, want running", got)
	}
	f.requireAttemptOpen(t)
}
