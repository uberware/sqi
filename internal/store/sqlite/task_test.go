// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestRetryTasks_SQLite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusFailed); err != nil {
		t.Fatalf("UpdateJobStatus failed: %v", err)
	}
	insertStep(t, s, "s1", "j1", "S1", 0)
	if err := s.UpdateStepStatus(ctx, "s1", store.StepStatusFailed); err != nil {
		t.Fatalf("UpdateStepStatus failed: %v", err)
	}
	insertTask(t, s, "t-failed", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "t-failed", store.TaskStatusFailed); err != nil {
		t.Fatalf("UpdateTaskStatus t-failed: %v", err)
	}
	insertTask(t, s, "t-canceled", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "t-canceled", store.TaskStatusCanceled); err != nil {
		t.Fatalf("UpdateTaskStatus t-canceled: %v", err)
	}
	insertTask(t, s, "t-ok", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "t-ok", store.TaskStatusSucceeded); err != nil {
		t.Fatalf("UpdateTaskStatus t-ok: %v", err)
	}

	revived, err := s.RetryTasks(ctx, "j1", nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("RetryTasks: %v", err)
	}
	if len(revived) != 2 {
		t.Fatalf("revived = %d, want 2", len(revived))
	}
	for _, rt := range revived {
		if rt.Status != store.TaskStatusPending {
			t.Errorf("returned revived task %q has status %v, want pending", rt.ID, rt.Status)
		}
	}

	for _, id := range []string{"t-failed", "t-canceled"} {
		tk, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", id, err)
		}
		if tk.Status != store.TaskStatusPending {
			t.Errorf("%s = %v, want pending", id, tk.Status)
		}
	}
	ok, err := s.GetTask(ctx, "t-ok")
	if err != nil {
		t.Fatalf("GetTask(t-ok): %v", err)
	}
	if ok.Status != store.TaskStatusSucceeded {
		t.Errorf("t-ok = %v, want succeeded", ok.Status)
	}
	job, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob(j1): %v", err)
	}
	if job.Status != store.JobStatusPending {
		t.Errorf("job = %v, want pending", job.Status)
	}
	step, err := s.GetStep(ctx, "s1")
	if err != nil {
		t.Fatalf("GetStep(s1): %v", err)
	}
	if step.Status != store.StepStatusPending {
		t.Errorf("step = %v, want pending", step.Status)
	}

	// Subset filter + idempotent empty result.
	again, err := s.RetryTasks(ctx, "j1", []string{"t-ok"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("RetryTasks subset: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("revived = %d, want 0 (t-ok not retryable)", len(again))
	}
}

// TestRetryTasks_EmptySliceRevivesNothing asserts that a non-nil but empty
// taskIDs slice revives nothing and leaves failed tasks untouched.
func TestRetryTasks_EmptySliceRevivesNothing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusFailed); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	insertStep(t, s, "s1", "j1", "S1", 0)
	if err := s.UpdateStepStatus(ctx, "s1", store.StepStatusFailed); err != nil {
		t.Fatalf("UpdateStepStatus: %v", err)
	}
	insertTask(t, s, "t-failed", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "t-failed", store.TaskStatusFailed); err != nil {
		t.Fatalf("UpdateTaskStatus t-failed: %v", err)
	}

	// Non-nil but empty slice: "filter to exactly these (zero) IDs" → revive nothing.
	revived, err := s.RetryTasks(ctx, "j1", []string{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("RetryTasks(empty slice): unexpected error: %v", err)
	}
	if len(revived) != 0 {
		t.Errorf("revived = %d, want 0 (empty filter must revive nothing)", len(revived))
	}

	// The failed task must remain failed.
	tk, err := s.GetTask(ctx, "t-failed")
	if err != nil {
		t.Fatalf("GetTask(t-failed): %v", err)
	}
	if tk.Status != store.TaskStatusFailed {
		t.Errorf("t-failed = %v, want failed (must not be revived)", tk.Status)
	}
}

// TestRetryTasks_MixedStateStep tests the documented behavior when a subset
// taskIDs filter is applied to a step that has both failed and non-failed tasks:
// only the requested task is revived, the sibling stays failed, and the step is
// reset to pending because it now owns a pending task.
func TestRetryTasks_MixedStateStep(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusFailed); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	insertStep(t, s, "s1", "j1", "S1", 0)
	if err := s.UpdateStepStatus(ctx, "s1", store.StepStatusFailed); err != nil {
		t.Fatalf("UpdateStepStatus: %v", err)
	}
	insertTask(t, s, "ta", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "ta", store.TaskStatusFailed); err != nil {
		t.Fatalf("UpdateTaskStatus ta: %v", err)
	}
	insertTask(t, s, "tb", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "tb", store.TaskStatusFailed); err != nil {
		t.Fatalf("UpdateTaskStatus tb: %v", err)
	}

	// Retry only "ta" from the subset.
	revived, err := s.RetryTasks(ctx, "j1", []string{"ta"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("RetryTasks: %v", err)
	}
	if len(revived) != 1 {
		t.Fatalf("revived = %d, want 1", len(revived))
	}
	if revived[0].ID != "ta" {
		t.Errorf("revived[0].ID = %q, want ta", revived[0].ID)
	}
	if revived[0].Status != store.TaskStatusPending {
		t.Errorf("revived[0].Status = %v, want pending", revived[0].Status)
	}

	ta, err := s.GetTask(ctx, "ta")
	if err != nil {
		t.Fatalf("GetTask(ta): %v", err)
	}
	if ta.Status != store.TaskStatusPending {
		t.Errorf("ta = %v, want pending", ta.Status)
	}
	tb, err := s.GetTask(ctx, "tb")
	if err != nil {
		t.Fatalf("GetTask(tb): %v", err)
	}
	if tb.Status != store.TaskStatusFailed {
		t.Errorf("tb = %v, want failed (sibling untouched)", tb.Status)
	}
	// The step should be reset to pending because it now owns a pending task.
	step, err := s.GetStep(ctx, "s1")
	if err != nil {
		t.Fatalf("GetStep(s1): %v", err)
	}
	if step.Status != store.StepStatusPending {
		t.Errorf("step = %v, want pending (has a pending task now)", step.Status)
	}
}
