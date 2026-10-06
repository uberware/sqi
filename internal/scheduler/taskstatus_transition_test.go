// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// The task status consumer must ACK a message whose transition the store
// rejects, never Nak it.
//
// The store enforces the task state machine, so a stale or out-of-order
// worker message can now legitimately fail. task.status is a JetStream subject
// (at-least-once), and handleTaskStatusMessage Naks on error — so treating an
// invalid transition as retryable would redeliver the same doomed message
// forever. Redelivery cannot fix a transition that is illegal, exactly as it
// cannot fix a malformed payload.

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestHandleTaskStatusMessage_InvalidTransitionIsAcked drives a "canceled"
// message at a task that has already succeeded — the shape of a redelivered
// message arriving after the task reached a terminal state. (A "failed" one
// would not reach the state machine: its attempt is already closed, so the
// failure path discards it first as a stale report.)
func TestHandleTaskStatusMessage_InvalidTransitionIsAcked(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	// Drive the task to a terminal state first.
	exit := 0
	if res, err := st.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
		AttemptID: attempt.ID, TaskID: task.ID, TaskStatus: store.TaskStatusSucceeded,
		AttemptStatus: store.AttemptStatusSucceeded, ExitCode: &exit, EndedAt: time.Now().UTC(),
	}); err != nil || !res.Applied {
		t.Fatalf("CompleteTaskAttempt(running → succeeded) = (%+v, %v), want applied", res, err)
	}

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "canceled",
		}),
	}
	s.handleTaskStatusMessage(msg)

	if msg.nacked {
		t.Error("invalid transition was Naked — it would redeliver forever")
	}
	if !msg.acked {
		t.Error("invalid transition should be acked (discarded); redelivery cannot fix it")
	}

	// The terminal status must be untouched by the rejected message.
	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusSucceeded {
		t.Errorf("status = %q after rejected message, want succeeded (unchanged)", stored.Status)
	}
}

// TestHandleTaskStatusMessage_DuplicateRunningIsAcked covers the ordinary
// at-least-once case: the same "running" message delivered twice must ack both
// times, because a same-status write is a no-op rather than an error.
func TestHandleTaskStatusMessage_DuplicateRunningIsAcked(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusAssigned)

	for i := range 2 {
		msg := &fakeJSMsg{
			subject: statusTestSubject,
			data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
				Version:   protocol.ProtocolVersion,
				TaskID:    task.ID,
				AttemptID: attempt.ID,
				Status:    "running",
			}),
		}
		s.handleTaskStatusMessage(msg)

		if msg.nacked {
			t.Fatalf("delivery %d: running message was Naked", i+1)
		}
		if !msg.acked {
			t.Fatalf("delivery %d: running message should be acked", i+1)
		}
	}

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusRunning {
		t.Errorf("status = %q, want running", stored.Status)
	}
}
