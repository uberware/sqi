// SPDX-License-Identifier: AGPL-3.0-or-later

package restore_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/restore"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestFence_RequeuesEveryOpenAttemptWithoutARetry(t *testing.T) {
	st := fake.New()
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "f1", Name: "f"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(t.Context(), store.Queue{ID: "q1", FarmID: "f1", Name: "q"}); err != nil {
		t.Fatal(err)
	}
	_, attempts := storetest.NewJob("j1", "f1", "q1").
		Step("s1", store.StepStatusReady).
		Task("t1", "s1", store.TaskStatusRunning).
		Task("t2", "s1", store.TaskStatusAssigned).
		SubmitLeasing(t, st, "w1")
	if len(attempts) != 2 {
		t.Fatalf("seeded %d attempts, want 2", len(attempts))
	}

	n, err := restore.Fence(t.Context(), st, time.Now().UTC())
	if err != nil || n != 2 {
		t.Fatalf("Fence = %d, %v; want 2, nil", n, err)
	}
	for _, id := range []string{"t1", "t2"} {
		task, err := st.GetTask(t.Context(), id)
		if err != nil || task.Status != store.TaskStatusReady || task.FailedAttempts != 0 {
			t.Fatalf("task %s = %+v, %v; want ready with no failed attempt consumed", id, task, err)
		}
	}
	if n, err := restore.Fence(t.Context(), st, time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("second Fence = %d, %v; want 0 (idempotent)", n, err)
	}
}
