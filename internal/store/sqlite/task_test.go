// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
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
	walkTaskTo(t, s, "t-failed", store.TaskStatusFailed)
	insertTask(t, s, "t-canceled", "j1", "s1")
	walkTaskTo(t, s, "t-canceled", store.TaskStatusCanceled)
	insertTask(t, s, "t-ok", "j1", "s1")
	walkTaskTo(t, s, "t-ok", store.TaskStatusSucceeded)

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
	walkTaskTo(t, s, "t-failed", store.TaskStatusFailed)

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
	walkTaskTo(t, s, "ta", store.TaskStatusFailed)
	insertTask(t, s, "tb", "j1", "s1")
	walkTaskTo(t, s, "tb", store.TaskStatusFailed)

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
	walkTaskTo(t, s, "t1", store.TaskStatusRunning)

	// Drive genuine-failure bookkeeping: a failed attempt bumps both counters
	// and stamps a backoff; enough failures park the job with a reason.
	att := insertAttempt(t, s, "t1", "w1", 1)
	if _, _, _, err := s.RecordTaskFailure(ctx, att.ID, "t1", nil, "", "", now); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if requeued, err := s.RequeueTaskForRetry(ctx, "t1", now.Add(time.Minute), now); err != nil || !requeued {
		t.Fatalf("RequeueTaskForRetry: requeued=%v err=%v", requeued, err)
	}
	if err := s.ParkJob(ctx, "j1", "failure limit reached (1)", now); err != nil {
		t.Fatalf("ParkJob: %v", err)
	}

	// Drive the task and job to the terminal states RetryTasks operates on
	// (park leaves the job paused, not terminal — so move both to failed
	// directly, as the production failure sweep would eventually do).
	walkTaskTo(t, s, "t1", store.TaskStatusFailed)
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

// TestRetryTasks_ClearsFailureReason asserts that a manual retry via RetryTasks
// clears a task's stale failure_reason (Task 4) — a revived task must not carry
// forward the reason from its prior terminal failure.
func TestRetryTasks_ClearsFailureReason(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusFailed); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	walkTaskTo(t, s, "t1", store.TaskStatusFailed)
	if err := s.SetTaskFailureReason(ctx, "t1", "boom"); err != nil {
		t.Fatalf("SetTaskFailureReason: %v", err)
	}

	if _, err := s.RetryTasks(ctx, "j1", nil, time.Now().UTC()); err != nil {
		t.Fatalf("RetryTasks: %v", err)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FailureReason != "" {
		t.Fatalf("manual retry did not clear failure_reason: %q", got.FailureReason)
	}
}

// recordFailureFixture seeds a running farm/queue/job/step/task and returns the
// store, ctx, and a now stamp — the shared setup for the RecordTaskFailure
// tests below.
func recordFailureFixture(t *testing.T) (*sqlite.Store, context.Context, time.Time) {
	t.Helper()
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
	return s, ctx, now
}

// TestRecordTaskFailure_CountsEachAttempt asserts that RecordTaskFailure
// increments the task's and job's failed_attempts counters once per DISTINCT
// attempt: two genuine attempts of the same task raise both counters to two.
func TestRecordTaskFailure_CountsEachAttempt(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	a1 := insertAttempt(t, s, "t1", "w1", 1)
	tf, jf, first, err := s.RecordTaskFailure(ctx, a1.ID, "t1", nil, "", "", now)
	if err != nil || tf != 1 || jf != 1 || !first {
		t.Fatalf("first attempt: tf=%d jf=%d first=%v err=%v", tf, jf, first, err)
	}

	a2 := insertAttempt(t, s, "t1", "w1", 2)
	tf, jf, first, err = s.RecordTaskFailure(ctx, a2.ID, "t1", nil, "", "", now)
	if err != nil || tf != 2 || jf != 2 || !first {
		t.Fatalf("second attempt: tf=%d jf=%d first=%v err=%v", tf, jf, first, err)
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

// TestRecordTaskFailure_IdempotentPerAttempt is the IMP-1 regression: because a
// worker's "failed" status message is delivered at-least-once, RecordTaskFailure
// must count exactly once per attempt. The first call closes the running
// attempt and increments both counters; a second call for the SAME attempt (a
// redelivery) finds the attempt already terminal, returns the SAME counts, and
// does NOT re-increment.
func TestRecordTaskFailure_IdempotentPerAttempt(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	att := insertAttempt(t, s, "t1", "w1", 1)
	exit := 7
	sess := "sess-1"

	// First delivery: closes the attempt as failed and counts once.
	tf, jf, first, err := s.RecordTaskFailure(ctx, att.ID, "t1", &exit, sess, "", now)
	if err != nil || tf != 1 || jf != 1 || !first {
		t.Fatalf("first delivery: tf=%d jf=%d first=%v err=%v", tf, jf, first, err)
	}

	closed, err := s.GetTaskAttempt(ctx, att.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if closed.Status != store.AttemptStatusFailed {
		t.Errorf("attempt status = %q, want failed", closed.Status)
	}
	if closed.EndedAt == nil {
		t.Error("attempt EndedAt not stamped on close")
	}
	if closed.ExitCode == nil || *closed.ExitCode != exit {
		t.Errorf("attempt ExitCode = %v, want %d", closed.ExitCode, exit)
	}
	if closed.SessionID != sess {
		t.Errorf("attempt SessionID = %q, want %q", closed.SessionID, sess)
	}

	// Redelivery: the attempt is already terminal, so the counts must NOT move
	// and firstClose must be false — the caller uses it to withhold the
	// retry/park actions from stale reports.
	tf, jf, first, err = s.RecordTaskFailure(ctx, att.ID, "t1", &exit, sess, "", now.Add(time.Second))
	if err != nil || tf != 1 || jf != 1 || first {
		t.Fatalf("redelivery: tf=%d jf=%d first=%v err=%v (want 1,1,false — no re-count)", tf, jf, first, err)
	}

	// Persisted counters incremented exactly once.
	task, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.FailedAttempts != 1 {
		t.Errorf("task.FailedAttempts = %d, want 1 (exactly once)", task.FailedAttempts)
	}
	job, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.FailedAttempts != 1 {
		t.Errorf("job.FailedAttempts = %d, want 1 (exactly once)", job.FailedAttempts)
	}
}

// TestRecordTaskFailure_NotFound asserts that recording a failure for an
// unknown task returns [store.ErrNotFound] and leaves nothing modified.
func TestRecordTaskFailure_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, _, _, err := s.RecordTaskFailure(ctx, "missing-attempt", "missing", nil, "", "", time.Now().UTC())
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
	if requeued, err := s.RequeueTaskForRetry(ctx, "t1", future, now); err != nil || !requeued {
		t.Fatalf("requeue: requeued=%v err=%v", requeued, err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusReady || got.AssignedWorkerID != "" || got.RetryAfter == nil {
		t.Fatalf("bad state: %+v", got)
	}
}

// TestRequeueTaskForRetry_ClearsFailureReason asserts that the auto-retry path
// clears a task's stale failure_reason (Task 4) — a requeued task must not
// carry forward the reason from its prior failed attempt.
func TestRequeueTaskForRetry_ClearsFailureReason(t *testing.T) {
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
	if err := s.SetTaskFailureReason(ctx, "t1", "boom"); err != nil {
		t.Fatalf("SetTaskFailureReason: %v", err)
	}

	if requeued, err := s.RequeueTaskForRetry(ctx, "t1", now.Add(30*time.Second), now); err != nil || !requeued {
		t.Fatalf("RequeueTaskForRetry: requeued=%v err=%v", requeued, err)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FailureReason != "" {
		t.Fatalf("auto retry did not clear failure_reason: %q", got.FailureReason)
	}
}

// TestRequeueTaskForRetry_GuardedToInFlight asserts the status guard: only an
// assigned/running task is requeued. A missing task and — critically — a task
// that has since been canceled, succeeded, or already returned to ready are
// legitimate no-ops (false, nil), never a resurrection: a stale or redelivered
// failure report must not flip a terminal task back to ready or clear its
// failure reason.
func TestRequeueTaskForRetry_GuardedToInFlight(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)

	if requeued, err := s.RequeueTaskForRetry(ctx, "missing", now.Add(time.Second), now); err != nil || requeued {
		t.Fatalf("missing task: requeued=%v err=%v, want false,nil", requeued, err)
	}

	for i, tc := range []struct {
		status store.TaskStatus
		reason string
	}{
		{store.TaskStatusCanceled, store.FailureReasonCanceledByUser},
		{store.TaskStatusSucceeded, ""},
		{store.TaskStatusReady, ""},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			id := "t" + string(rune('1'+i))
			insertTask(t, s, id, "j1", "s1")
			walkTaskTo(t, s, id, tc.status)
			if tc.reason != "" {
				if err := s.SetTaskFailureReason(ctx, id, tc.reason); err != nil {
					t.Fatalf("SetTaskFailureReason: %v", err)
				}
			}

			requeued, err := s.RequeueTaskForRetry(ctx, id, now.Add(time.Second), now)
			if err != nil || requeued {
				t.Fatalf("requeued=%v err=%v, want false,nil", requeued, err)
			}

			got, err := s.GetTask(ctx, id)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.Status != tc.status {
				t.Errorf("status = %q, want untouched %q", got.Status, tc.status)
			}
			if got.FailureReason != tc.reason {
				t.Errorf("failure_reason = %q, want untouched %q", got.FailureReason, tc.reason)
			}
			if got.RetryAfter != nil {
				t.Errorf("retry_after stamped on ineligible task")
			}
		})
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

// TestResumeJob_AutoParked_ClearsParkStateAndCounter asserts that resuming an
// auto-parked job clears park_reason AND resets the job's failure counter —
// re-arming the failure limit so the next genuine failure retries instead of
// instantly re-parking the job.
func TestResumeJob_AutoParked_ClearsParkStateAndCounter(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	// One genuine failure gives the job a nonzero counter, then park it.
	att := insertAttempt(t, s, "t1", "w1", 1)
	if _, _, _, err := s.RecordTaskFailure(ctx, att.ID, "t1", nil, "", "", now); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if err := s.ParkJob(ctx, "j1", "failure limit reached (1)", now); err != nil {
		t.Fatalf("ParkJob: %v", err)
	}

	if err := s.ResumeJob(ctx, "j1", now); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}

	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusPending {
		t.Errorf("status = %s, want pending", got.Status)
	}
	if got.ParkReason != "" {
		t.Errorf("park_reason = %q, want cleared", got.ParkReason)
	}
	if got.FailedAttempts != 0 {
		t.Errorf("failed_attempts = %d, want 0 (limit re-armed)", got.FailedAttempts)
	}
}

// TestResumeJob_ManualPause_KeepsCounter asserts that resuming a MANUALLY
// paused job (empty park_reason) does not touch its accumulated failure
// counter — only an auto-park reset is implied by resume.
func TestResumeJob_ManualPause_KeepsCounter(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	att := insertAttempt(t, s, "t1", "w1", 1)
	if _, _, _, err := s.RecordTaskFailure(ctx, att.ID, "t1", nil, "", "", now); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusPaused); err != nil {
		t.Fatalf("UpdateJobStatus(paused): %v", err)
	}

	if err := s.ResumeJob(ctx, "j1", now); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}

	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusPending {
		t.Errorf("status = %s, want pending", got.Status)
	}
	if got.FailedAttempts != 1 {
		t.Errorf("failed_attempts = %d, want 1 (manual resume keeps the count)", got.FailedAttempts)
	}
}

// TestResumeJob_NotPausedAndNotFound asserts the edge semantics: a job that is
// not paused is a legitimate no-op, an unknown job is ErrNotFound.
func TestResumeJob_NotPausedAndNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusRunning); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}

	if err := s.ResumeJob(ctx, "j1", now); err != nil {
		t.Fatalf("resume of non-paused job should be a no-op, got %v", err)
	}
	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusRunning {
		t.Errorf("non-paused job modified by resume: %+v", got)
	}

	if err := s.ResumeJob(ctx, "missing", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestRetryTasks_UnparksAutoParkedJob asserts the spec §4.5 contract: a manual
// retry of an AUTO-PARKED job (paused with a park_reason — not terminal)
// resets the job to pending and clears its failure counter and park reason,
// exactly as it does for a terminal job.
func TestRetryTasks_UnparksAutoParkedJob(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	att := insertAttempt(t, s, "t1", "w1", 1)
	if _, _, _, err := s.RecordTaskFailure(ctx, att.ID, "t1", nil, "", "", now); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	// The tripping task went terminal-failed and the job parked.
	walkTaskTo(t, s, "t1", store.TaskStatusFailed)
	if err := s.ParkJob(ctx, "j1", "failure limit reached (1)", now); err != nil {
		t.Fatalf("ParkJob: %v", err)
	}

	revived, err := s.RetryTasks(ctx, "j1", nil, now)
	if err != nil || len(revived) != 1 {
		t.Fatalf("RetryTasks: revived=%d err=%v", len(revived), err)
	}

	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusPending {
		t.Errorf("status = %s, want pending (retry un-parks)", got.Status)
	}
	if got.ParkReason != "" || got.FailedAttempts != 0 {
		t.Errorf("park state not cleared: reason=%q failed_attempts=%d", got.ParkReason, got.FailedAttempts)
	}
}

// TestRetryTasks_LeavesManualPauseAlone asserts the counterpart guard: a
// manually paused job (no park_reason) is NOT un-paused by retrying its tasks
// — the operator's pause outranks the retry.
func TestRetryTasks_LeavesManualPauseAlone(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	walkTaskTo(t, s, "t1", store.TaskStatusFailed)
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusPaused); err != nil {
		t.Fatalf("UpdateJobStatus(paused): %v", err)
	}

	if _, err := s.RetryTasks(ctx, "j1", nil, now); err != nil {
		t.Fatalf("RetryTasks: %v", err)
	}

	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusPaused {
		t.Errorf("manually paused job un-paused by retry: status = %s", got.Status)
	}
}

// TestSetTaskFailureReason_RoundTrip asserts that SetTaskFailureReason
// persists a reason and that a subsequent call with an empty string clears it.
func TestSetTaskFailureReason_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "t1", store.TaskStatusReady); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	if err := s.SetTaskFailureReason(ctx, "t1", "boom"); err != nil {
		t.Fatalf("SetTaskFailureReason: %v", err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FailureReason != "boom" {
		t.Fatalf("FailureReason = %q, want %q", got.FailureReason, "boom")
	}

	if err := s.SetTaskFailureReason(ctx, "t1", ""); err != nil {
		t.Fatalf("SetTaskFailureReason (clear): %v", err)
	}
	got, err = s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FailureReason != "" {
		t.Fatalf("FailureReason not cleared: %q", got.FailureReason)
	}
}

// TestSetTaskFailureReason_NotFound asserts that setting the failure reason of
// an unknown task returns [store.ErrNotFound].
func TestSetTaskFailureReason_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.SetTaskFailureReason(ctx, "missing", "x")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestSetTaskFailureReasonIfEmpty asserts the guarded setter writes when the
// task has no reason yet and is a no-op (preserving the existing reason) when
// one is already recorded — the invariant that lets a cascade-cancel reason
// survive a later user-cancel.
func TestSetTaskFailureReasonIfEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	// Empty → sets.
	if err := s.SetTaskFailureReasonIfEmpty(ctx, "t1", "canceled by user"); err != nil {
		t.Fatalf("SetTaskFailureReasonIfEmpty (empty): %v", err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FailureReason != "canceled by user" {
		t.Fatalf("FailureReason = %q, want %q", got.FailureReason, "canceled by user")
	}

	// Non-empty → no-op, existing reason preserved.
	if err := s.SetTaskFailureReasonIfEmpty(ctx, "t1", "something else"); err != nil {
		t.Fatalf("SetTaskFailureReasonIfEmpty (non-empty): %v", err)
	}
	got, err = s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FailureReason != "canceled by user" {
		t.Fatalf("FailureReason overwritten: got %q, want %q", got.FailureReason, "canceled by user")
	}

	// Unknown task → legitimate no-op, not an error.
	if err := s.SetTaskFailureReasonIfEmpty(ctx, "missing", "x"); err != nil {
		t.Fatalf("SetTaskFailureReasonIfEmpty (missing) = %v, want nil", err)
	}
}

// TestTaskAttempt_MessageRoundTrip asserts that a TaskAttempt's Message
// persists through CreateTaskAttempt and can be set via UpdateTaskAttempt.
func TestTaskAttempt_MessageRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	a := insertAttempt(t, s, "t1", "w1", 1)
	if a.Message != "" {
		t.Fatalf("new attempt Message = %q, want empty", a.Message)
	}

	a.Status = store.AttemptStatusFailed
	end := time.Now().UTC()
	a.EndedAt = &end
	a.Message = "execution timeout after 120s"
	updated, err := s.UpdateTaskAttempt(ctx, a)
	if err != nil {
		t.Fatalf("UpdateTaskAttempt: %v", err)
	}
	if updated.Message != "execution timeout after 120s" {
		t.Fatalf("updated.Message = %q, want %q", updated.Message, "execution timeout after 120s")
	}

	got, err := s.GetTaskAttempt(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if got.Message != "execution timeout after 120s" {
		t.Fatalf("GetTaskAttempt Message = %q, want %q", got.Message, "execution timeout after 120s")
	}
}

// failedTaskWithReason creates a task in [store.TaskStatusFailed] carrying
// the given failure_reason.
func failedTaskWithReason(t *testing.T, s *sqlite.Store, id, jobID, stepID, reason string) {
	t.Helper()
	insertTask(t, s, id, jobID, stepID)
	ctx := context.Background()
	walkTaskTo(t, s, id, store.TaskStatusFailed)
	if err := s.SetTaskFailureReason(ctx, id, reason); err != nil {
		t.Fatalf("SetTaskFailureReason(%q): %v", id, err)
	}
}

// TestFailureReasonSummary_Mixed asserts the summary counts failed tasks
// grouped by reason and picks the most frequent reason as dominant.
func TestFailureReasonSummary_Mixed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)

	failedTaskWithReason(t, s, "t0", "j1", "s1", "staging")
	failedTaskWithReason(t, s, "t1", "j1", "s1", "staging")
	failedTaskWithReason(t, s, "t2", "j1", "s1", "timeout")

	sum, err := s.FailureReasonSummary(ctx, "j1")
	if err != nil {
		t.Fatalf("FailureReasonSummary: %v", err)
	}
	if sum.FailedCount != 3 || sum.DominantReason != "staging" || sum.DistinctReasons != 2 {
		t.Fatalf("got %+v, want {FailedCount:3 DominantReason:staging DistinctReasons:2}", sum)
	}
}

// TestFailureReasonSummary_Tie asserts that when two reasons tie on
// frequency, the dominant reason is the lexicographically smaller one —
// deterministic regardless of insertion order.
func TestFailureReasonSummary_Tie(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)

	failedTaskWithReason(t, s, "t0", "j1", "s1", "timeout")
	failedTaskWithReason(t, s, "t1", "j1", "s1", "staging")

	sum, err := s.FailureReasonSummary(ctx, "j1")
	if err != nil {
		t.Fatalf("FailureReasonSummary: %v", err)
	}
	if sum.FailedCount != 2 || sum.DominantReason != "staging" || sum.DistinctReasons != 2 {
		t.Fatalf("got %+v, want {FailedCount:2 DominantReason:staging DistinctReasons:2}", sum)
	}
}

// TestFailureReasonSummary_Empty asserts a job with no failed tasks (or no
// failure_reason recorded) returns the zero-value summary and no error.
func TestFailureReasonSummary_Empty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t0", "j1", "s1")
	walkTaskTo(t, s, "t0", store.TaskStatusSucceeded)

	sum, err := s.FailureReasonSummary(ctx, "j1")
	if err != nil {
		t.Fatalf("FailureReasonSummary: %v", err)
	}
	if (sum != store.FailureSummary{}) {
		t.Fatalf("got %+v, want zero value", sum)
	}
}
