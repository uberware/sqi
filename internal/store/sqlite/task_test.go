// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"errors"
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

// TestRetryTasks_ResetsFailureCounters asserts that a manual retry via
// RetryTasks clears the genuine-failure state Tasks 1-3 introduced: a revived
// task's FailedAttempts and RetryAfter are zeroed/cleared, and — when the
// retry resets a terminal job back to pending — the job's FailedAttempts and
// ParkReason are cleared too.
func TestRetryTasks_ResetsFailureCounters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusRunning); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "t1", store.TaskStatusReady); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	// Drive genuine-failure bookkeeping: a failed attempt bumps both counters
	// and stamps a backoff; enough failures park the job with a reason.
	if _, _, err := s.RecordTaskFailure(ctx, "t1", now); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if err := s.RequeueTaskForRetry(ctx, "t1", now.Add(time.Minute), now); err != nil {
		t.Fatalf("RequeueTaskForRetry: %v", err)
	}
	if err := s.ParkJob(ctx, "j1", "failure limit reached (1)", now); err != nil {
		t.Fatalf("ParkJob: %v", err)
	}

	// Drive the task and job to the terminal states RetryTasks operates on
	// (park leaves the job paused, not terminal — so move both to failed
	// directly, as the production failure sweep would eventually do).
	if err := s.UpdateTaskStatus(ctx, "t1", store.TaskStatusFailed); err != nil {
		t.Fatalf("UpdateTaskStatus(failed): %v", err)
	}
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusFailed); err != nil {
		t.Fatalf("UpdateJobStatus(failed): %v", err)
	}

	// Sanity-check the fixture actually has nonzero state before retrying.
	preTask, err := s.GetTask(ctx, "t1")
	if err != nil || preTask.FailedAttempts == 0 || preTask.RetryAfter == nil {
		t.Fatalf("pre-retry task fixture not as expected: %+v err=%v", preTask, err)
	}
	preJob, err := s.GetJob(ctx, "j1")
	if err != nil || preJob.FailedAttempts == 0 || preJob.ParkReason == "" {
		t.Fatalf("pre-retry job fixture not as expected: %+v err=%v", preJob, err)
	}

	revived, err := s.RetryTasks(ctx, "j1", nil, now)
	if err != nil || len(revived) == 0 {
		t.Fatalf("RetryTasks: %v revived=%d", err, len(revived))
	}

	task, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.FailedAttempts != 0 || task.RetryAfter != nil {
		t.Fatalf("task counters not reset: %+v", task)
	}

	job, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.FailedAttempts != 0 || job.ParkReason != "" {
		t.Fatalf("job counters not reset: %+v", job)
	}
}

// TestRecordTaskFailure_IncrementsBothCounters asserts that RecordTaskFailure
// atomically increments a task's and its job's failed_attempts counters,
// returning the post-increment values on each call.
func TestRecordTaskFailure_IncrementsBothCounters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusRunning); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "t1", store.TaskStatusReady); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	tf, jf, err := s.RecordTaskFailure(ctx, "t1", now)
	if err != nil || tf != 1 || jf != 1 {
		t.Fatalf("first: tf=%d jf=%d err=%v", tf, jf, err)
	}
	tf, jf, err = s.RecordTaskFailure(ctx, "t1", now)
	if err != nil || tf != 2 || jf != 2 {
		t.Fatalf("second: tf=%d jf=%d err=%v", tf, jf, err)
	}

	// Persisted state matches the returned counters.
	task, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.FailedAttempts != 2 {
		t.Errorf("task.FailedAttempts = %d, want 2", task.FailedAttempts)
	}
	job, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.FailedAttempts != 2 {
		t.Errorf("job.FailedAttempts = %d, want 2", job.FailedAttempts)
	}
}

// TestRecordTaskFailure_NotFound asserts that recording a failure for an
// unknown task returns [store.ErrNotFound] and leaves nothing modified.
func TestRecordTaskFailure_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, _, err := s.RecordTaskFailure(ctx, "missing", time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestRequeueTaskForRetry_ResetsAssignment asserts that RequeueTaskForRetry
// returns an assigned task to ready, clears its worker assignment, and stamps
// retry_after with the supplied backoff time.
func TestRequeueTaskForRetry_ResetsAssignment(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertWorker(t, s, "w1", "f1")
	insertTask(t, s, "t1", "j1", "s1")
	if err := s.AssignTask(ctx, "t1", "w1", now); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}

	future := now.Add(30 * time.Second)
	if err := s.RequeueTaskForRetry(ctx, "t1", future, now); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusReady || got.AssignedWorkerID != "" || got.RetryAfter == nil {
		t.Fatalf("bad state: %+v", got)
	}
}

// TestRequeueTaskForRetry_NotFound asserts that requeuing an unknown task
// returns [store.ErrNotFound].
func TestRequeueTaskForRetry_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	err := s.RequeueTaskForRetry(ctx, "missing", now.Add(time.Second), now)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestParkJob_SkipsTerminal asserts that ParkJob transitions a non-terminal
// job to paused with a reason, but is a no-op (not an error) once the job has
// reached a terminal status.
func TestParkJob_SkipsTerminal(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusRunning); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}

	if err := s.ParkJob(ctx, "j1", "failure limit reached (2)", now); err != nil {
		t.Fatalf("park: %v", err)
	}
	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusPaused || got.ParkReason == "" {
		t.Fatalf("bad park: %+v", got)
	}

	// A terminal job is left untouched — no error, no state change.
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusFailed); err != nil {
		t.Fatalf("UpdateJobStatus(failed): %v", err)
	}
	if err := s.ParkJob(ctx, "j1", "x", now); err != nil {
		t.Fatalf("park terminal (expected no-op, no error): %v", err)
	}
	got, err = s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusFailed {
		t.Fatalf("terminal job should not be parked: %+v", got)
	}
}

// TestParkJob_NotFound asserts that parking an unknown job returns
// [store.ErrNotFound].
func TestParkJob_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.ParkJob(ctx, "missing", "x", time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
