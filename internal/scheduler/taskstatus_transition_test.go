// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// The task status consumer must ACK a message whose transition the store
// rejects, never Nak it.
//
// UpdateTaskStatus enforces the task state machine, so a stale or out-of-order
// worker message can now legitimately fail. task.status is a JetStream subject
// (at-least-once), and handleTaskStatusMessage Naks on error — so treating an
// invalid transition as retryable would redeliver the same doomed message
// forever. Redelivery cannot fix a transition that is illegal, exactly as it
// cannot fix a malformed payload.

import (
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// TestHandleTaskStatusMessage_InvalidTransitionIsAcked drives a "failed"
// message at a task that has already succeeded — the shape of a redelivered
// message arriving after the task reached a terminal state.
func TestHandleTaskStatusMessage_InvalidTransitionIsAcked(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	// Drive the task to a terminal state first.
	if err := st.UpdateTaskStatus(t.Context(), task.ID, store.TaskStatusSucceeded); err != nil {
		t.Fatalf("UpdateTaskStatus(running → succeeded): %v", err)
	}

	msg := &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "failed",
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
	st := fake.New()
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
