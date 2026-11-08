// SPDX-License-Identifier: AGPL-3.0-or-later

package storetest

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// SeedRunTimes seeds one job whose tasks cover every case of a task's stored
// run time (tasks.run_ms): measured, unmeasurable for each reason, and not
// succeeded at all. It returns each task's ID with the run_ms it must hold:
// the duration of its latest attempt (highest attempt_number, then greatest
// ID) in milliseconds, when the task succeeded and that attempt succeeded with
// an ended_at no earlier than its started_at; nil otherwise. A migration test
// seeds this, rewinds past the column, migrates up and compares.
func SeedRunTimes(t testing.TB, st store.Store) map[string]*int64 {
	t.Helper()
	ctx := t.Context()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	step := "rt-" + uuid.NewString()
	id := func(name string) string { return name + "-" + uuid.NewString() }
	ok, frac, noEnd, skew, sup, tied, ready := id("ok"), id("frac"), id("noend"), id("skew"), id("sup"), id("tied"), id("ready")
	NewJob(uuid.NewString(), farm.ID, queue.ID).As(store.JobStatusRunning).
		Step(step, store.StepStatusReady).
		Task(ok, step, store.TaskStatusSucceeded).
		Task(frac, step, store.TaskStatusSucceeded).
		Task(noEnd, step, store.TaskStatusSucceeded).
		Task(skew, step, store.TaskStatusSucceeded).
		Task(sup, step, store.TaskStatusSucceeded).
		Task(tied, step, store.TaskStatusSucceeded).
		Task(ready, step, store.TaskStatusReady).
		Submit(t, st)

	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	inject := func(attemptID, taskID string, num int, status store.AttemptStatus, end *time.Time) {
		t.Helper()
		InjectAttempt(t, st, store.TaskAttempt{
			ID: attemptID, TaskID: taskID, WorkerID: "w-runms", AttemptNumber: num,
			Status: status, StartedAt: t0, EndedAt: end,
		})
	}
	inject(uuid.NewString(), ok, 1, store.AttemptStatusSucceeded, at(time.Minute))
	inject(uuid.NewString(), frac, 1, store.AttemptStatusSucceeded, at(1500*time.Millisecond))
	inject(uuid.NewString(), noEnd, 1, store.AttemptStatusSucceeded, nil)
	inject(uuid.NewString(), skew, 1, store.AttemptStatusSucceeded, at(-30*time.Second))
	inject(uuid.NewString(), sup, 1, store.AttemptStatusSucceeded, at(30*time.Second))
	inject(uuid.NewString(), sup, 2, store.AttemptStatusFailed, at(2*time.Minute))
	inject("a-"+uuid.NewString(), tied, 1, store.AttemptStatusSucceeded, at(20*time.Second))
	inject("b-"+uuid.NewString(), tied, 1, store.AttemptStatusSucceeded, at(70*time.Second))
	inject(uuid.NewString(), ready, 1, store.AttemptStatusSucceeded, at(time.Minute))

	ms := func(v int64) *int64 { return &v }
	return map[string]*int64{
		ok: ms(60_000), frac: ms(1_500), noEnd: nil, skew: nil, sup: nil, tied: ms(70_000), ready: nil,
	}
}
