// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// countingStore wraps a store, counting the calls a progress snapshot must not
// make and optionally failing RecordAttemptProgress.
type countingStore struct {
	store.Store

	starts, gets int
	recordErr    error
}

func (c *countingStore) StartTaskAttempt(
	ctx context.Context, attemptID, taskID, sessionID string, now time.Time,
) (bool, error) {
	c.starts++
	return c.Store.StartTaskAttempt(ctx, attemptID, taskID, sessionID, now)
}

func (c *countingStore) GetTask(ctx context.Context, id string) (store.Task, error) {
	c.gets++
	return c.Store.GetTask(ctx, id)
}

func (c *countingStore) RecordAttemptProgress(
	ctx context.Context, p store.AttemptProgress,
) (store.ProgressOutcome, error) {
	if c.recordErr != nil {
		return store.ProgressNotLive, c.recordErr
	}
	return c.Store.RecordAttemptProgress(ctx, p)
}

func snapshotMsg(t *testing.T, task store.Task, attempt store.TaskAttempt, pct int, text string, at time.Time) *fakeJSMsg {
	t.Helper()
	return &fakeJSMsg{
		subject: statusTestSubject,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:      protocol.ProtocolVersion,
			JobID:        task.JobID,
			TaskID:       task.ID,
			AttemptID:    attempt.ID,
			Status:       "running",
			LastProgress: &pct,
			Message:      text,
			At:           at,
		}),
	}
}

func TestProcessTaskStatus_RunningSnapshot_Recorded(t *testing.T) {
	st := newCheckedFake(t)
	n := &recordingNotifier{}
	b := &stubBus{}
	s := newTestScheduler(st, b)
	s.notifier = n
	s.ctx = t.Context()
	job, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	msg := snapshotMsg(t, task, attempt, 42, "frame 3", t0)
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected ack")
	}
	a, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Progress == nil || *a.Progress != 42 || a.StatusText != "frame 3" || a.ProgressAt == nil || !a.ProgressAt.Equal(t0) {
		t.Errorf("attempt = %+v, want 42 / frame 3 / %v", a, t0)
	}
	if len(n.tasks) != 1 {
		t.Fatalf("events = %d, want 1", len(n.tasks))
	}
	e := n.tasks[0]
	if e.JobID != job.ID || e.TaskID != task.ID || e.Status != "running" ||
		e.Progress == nil || *e.Progress != 42 || e.StatusText != "frame 3" {
		t.Errorf("event = %+v", e)
	}
	if len(b.cancels) != 0 {
		t.Errorf("cancels = %v, want none", b.cancels)
	}
}

func TestProcessTaskStatus_RunningSnapshot_SkipsStartAndLookups(t *testing.T) {
	cs := &countingStore{Store: newCheckedFake(t)}
	s := newTestScheduler(cs, &stubBus{})
	s.ctx = t.Context()
	_, _, task, attempt := seedStatusFixture(t, cs.Store, store.TaskStatusRunning)

	s.handleTaskStatusMessage(snapshotMsg(t, task, attempt, 10, "x", time.Now().UTC()))

	if cs.starts != 0 || cs.gets != 0 {
		t.Errorf("StartTaskAttempt calls = %d, GetTask calls = %d, want 0, 0", cs.starts, cs.gets)
	}
}

func TestProcessTaskStatus_RunningSnapshot_OutOfOrder(t *testing.T) {
	st := newCheckedFake(t)
	n := &recordingNotifier{}
	s := newTestScheduler(st, &stubBus{})
	s.notifier = n
	s.ctx = t.Context()
	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	s.handleTaskStatusMessage(snapshotMsg(t, task, attempt, 42, "new", t0))
	msg := snapshotMsg(t, task, attempt, 5, "old", t0.Add(-time.Second))
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected ack")
	}
	a, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Progress == nil || *a.Progress != 42 || a.StatusText != "new" {
		t.Errorf("attempt = %+v, want unchanged 42 / new", a)
	}
	if len(n.tasks) != 1 {
		t.Errorf("events = %d, want 1", len(n.tasks))
	}
}

func TestProcessTaskStatus_RunningSnapshot_AssignedTask(t *testing.T) {
	st := newCheckedFake(t)
	n := &recordingNotifier{}
	s := newTestScheduler(st, &stubBus{})
	s.notifier = n
	s.ctx = t.Context()
	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusAssigned)
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	msg := snapshotMsg(t, task, attempt, 7, "warming", t0)
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected ack")
	}
	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.TaskStatusRunning {
		t.Errorf("task status = %q, want running", stored.Status)
	}
	a, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Progress == nil || *a.Progress != 7 || a.StatusText != "warming" {
		t.Errorf("attempt = %+v, want 7 / warming", a)
	}
	if len(n.tasks) != 1 {
		t.Fatalf("events = %d, want 1", len(n.tasks))
	}
	if e := n.tasks[0]; e.Progress == nil || *e.Progress != 7 || e.StatusText != "warming" {
		t.Errorf("event = %+v, want progress fields", e)
	}
}

func TestProcessTaskStatus_RunningSnapshot_StaleAttempt(t *testing.T) {
	st := newCheckedFake(t)
	n := &recordingNotifier{}
	b := &stubBus{}
	s := newTestScheduler(st, b)
	s.notifier = n
	s.ctx = t.Context()
	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
	if ok, err := st.ReclaimTaskAttempt(t.Context(), attempt.ID, task.ID, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("ReclaimTaskAttempt = %v, %v", ok, err)
	}

	msg := snapshotMsg(t, task, attempt, 50, "late", time.Now().UTC())
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected ack")
	}
	if len(b.cancels) != 1 || b.cancels[0].AttemptID != attempt.ID {
		t.Errorf("cancels = %v, want one for attempt %s", b.cancels, attempt.ID)
	}
	if len(n.tasks) != 0 {
		t.Errorf("events = %d, want 0", len(n.tasks))
	}
}

func TestProcessTaskStatus_RunningSnapshot_Fenced(t *testing.T) {
	cs := &countingStore{Store: newCheckedFake(t), recordErr: fmt.Errorf("x: %w", store.ErrFenced)}
	s := newTestScheduler(cs, &stubBus{})
	s.ctx = t.Context()
	_, _, task, attempt := seedStatusFixture(t, cs.Store, store.TaskStatusRunning)

	msg := snapshotMsg(t, task, attempt, 50, "x", time.Now().UTC())
	s.handleTaskStatusMessage(msg)

	if msg.acked || msg.nacked {
		t.Errorf("acked=%v nacked=%v, want neither", msg.acked, msg.nacked)
	}
}
