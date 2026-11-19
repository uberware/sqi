// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

func TestTerminalEndedAt(t *testing.T) {
	s0 := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) // broker stamp
	start := s0.Add(-time.Minute)                        // lease
	tests := []struct {
		name             string
		stored, received time.Time
		agoMs            int64
		want             time.Time
	}{
		{"no-age", s0, s0.Add(10 * time.Millisecond), 0, s0},
		{"retried-30s", s0, s0.Add(time.Second), 30_000, s0.Add(-30 * time.Second)},
		{"age-before-lease-clamped", s0, s0, 10 * 60_000, start},
		{"ahead-clamped-to-receipt", s0.Add(time.Minute), s0, 0, s0},
		{"negative-age-is-zero", s0, s0, -5_000, s0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminalEndedAt(tt.stored, tt.received, start, tt.agoMs); !got.Equal(tt.want) {
				t.Errorf("terminalEndedAt = %v, want %v", got, tt.want)
			}
		})
	}
}

// seedEndedAtFixture leases and starts one task 10 minutes before stamp, so a
// report stamped at stamp has room for its age.
func seedEndedAtFixture(t *testing.T, st store.Store, stamp time.Time) (store.Task, store.TaskAttempt) {
	t.Helper()
	g := seedStatusJob(t, st, statusJob{
		leasedAt: stamp.Add(-10 * time.Minute),
		steps: []statusStep{
			{name: "Step1", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusRunning}},
		},
	})
	task := g.tasks[0][0]
	return task, g.attempts[task.ID]
}

func endedAtSucceededMsg(t *testing.T, task store.Task, attempt store.TaskAttempt, stamp time.Time) *fakeJSMsg {
	t.Helper()
	exitCode := 0
	return &fakeJSMsg{
		subject: statusTestSubject,
		stamp:   stamp,
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version:    protocol.ProtocolVersion,
			TaskID:     task.ID,
			AttemptID:  attempt.ID,
			Status:     "succeeded",
			ExitCode:   &exitCode,
			At:         time.Now().UTC().Add(-10 * time.Minute), // a behind worker clock
			EndedAgoMs: 2000,
		}),
	}
}

func TestHandleTaskStatus_EndedAtFromBrokerStamp(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	stamp := time.Now().UTC().Add(-time.Minute)
	task, attempt := seedEndedAtFixture(t, st, stamp)

	msg := endedAtSucceededMsg(t, task, attempt, stamp)
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}
	got, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	want := stamp.Add(-2 * time.Second)
	if got.EndedAt == nil || !got.EndedAt.Equal(want) {
		t.Errorf("attempt EndedAt = %v, want %v", got.EndedAt, want)
	}
}

func TestHandleTaskStatus_LateDuplicateKeepsFirstEndedAt(t *testing.T) {
	st := newCheckedFake(t)
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	stamp := time.Now().UTC().Add(-time.Minute)
	task, attempt := seedEndedAtFixture(t, st, stamp)

	first := endedAtSucceededMsg(t, task, attempt, stamp)
	s.handleTaskStatusMessage(first)
	if !first.acked {
		t.Fatal("expected the first message to be acked")
	}

	second := endedAtSucceededMsg(t, task, attempt, stamp.Add(3*time.Minute))
	s.handleTaskStatusMessage(second)
	if !second.acked {
		t.Fatal("expected the late duplicate to be acked")
	}

	got, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	want := stamp.Add(-2 * time.Second)
	if got.EndedAt == nil || !got.EndedAt.Equal(want) {
		t.Errorf("attempt EndedAt = %v, want %v (the first report's)", got.EndedAt, want)
	}
	gotTask, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotTask.Status != store.TaskStatusSucceeded {
		t.Errorf("task status = %q, want succeeded", gotTask.Status)
	}
}
