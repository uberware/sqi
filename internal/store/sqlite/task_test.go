// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestRetryTasks_SQLite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").as(store.JobStatusFailed).
		stepAs("s1", "S1", 0, store.StepStatusFailed).
		task("t-failed", "s1", store.TaskStatusFailed).
		task("t-canceled", "s1", store.TaskStatusCanceled).
		task("t-ok", "s1", store.TaskStatusSucceeded).
		submit(t, s)

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
	newJob("j1", "f1", "q1").as(store.JobStatusFailed).
		stepAs("s1", "S1", 0, store.StepStatusFailed).
		task("t-failed", "s1", store.TaskStatusFailed).
		submit(t, s)

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
	newJob("j1", "f1", "q1").as(store.JobStatusFailed).
		stepAs("s1", "S1", 0, store.StepStatusFailed).
		task("ta", "s1", store.TaskStatusFailed).
		task("tb", "s1", store.TaskStatusFailed).
		submit(t, s)

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

func TestRetryTasks_ResetsFailureCounters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").as(store.JobStatusRunning).
		stepAs("s1", "S1", 0, store.StepStatusReady).
		task("t1", "s1", store.TaskStatusReady).
		submit(t, s)

	// Drive genuine-failure bookkeeping: a failed attempt bumps both counters
	// and stamps a backoff on the requeued task.
	att := leaseTask(t, s, "t1", "w1")
	if _, _, _, err := s.RecordTaskFailure(ctx, att.ID, "t1", nil, "", "", now); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if requeued, err := s.RequeueTaskForRetry(ctx, "t1", att.ID, now.Add(time.Minute), now); err != nil || !requeued {
		t.Fatalf("RequeueTaskForRetry: requeued=%v err=%v", requeued, err)
	}

	// The retry runs once the backoff has elapsed and fails for good, which
	// finalizes the step. The job is parked (enough failures park it with a
	// reason) and then finalized failed — the terminal state RetryTasks
	// operates on, reached the way the production failure sweep reaches it.
	retry := storetest.Running(t, s, store.LeaseRequest{TaskID: "t1", WorkerID: "w1", Now: now.Add(2 * time.Minute)})
	completeAttempt(t, s, retry, store.TaskStatusFailed, store.AttemptStatusFailed)
	if status, ok, err := s.FinalizeStep(ctx, "s1", now); err != nil || !ok || status != store.StepStatusFailed {
		t.Fatalf("FinalizeStep = (%v, %v, %v), want failed", status, ok, err)
	}
	if err := s.ParkJob(ctx, "j1", "failure limit reached (1)", now); err != nil {
		t.Fatalf("ParkJob: %v", err)
	}
	if status, ok, err := s.FinalizeJob(ctx, "j1", now); err != nil || !ok || status != store.JobStatusFailed {
		t.Fatalf("FinalizeJob = (%v, %v, %v), want failed", status, ok, err)
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
// clears a task's stale failure_reason — a revived task must not carry
// forward the reason from its prior terminal failure.
func TestRetryTasks_ClearsFailureReason(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").as(store.JobStatusFailed).step("s1", "S1", 0).
		taskRow(store.Task{ID: "t1", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "boom"}).
		submit(t, s)

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

func recordFailureFixture(t *testing.T) (*sqlite.Store, context.Context, time.Time) {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").as(store.JobStatusRunning).
		step("s1", "S1", 0).
		task("t1", "s1", store.TaskStatusReady).
		submit(t, s)
	return s, ctx, now
}

func TestRecordTaskFailure_CountsEachAttempt(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	a1 := leaseTask(t, s, "t1", "w1")
	tf, jf, first, err := s.RecordTaskFailure(ctx, a1.ID, "t1", nil, "", "", now)
	if err != nil || tf != 1 || jf != 1 || !first {
		t.Fatalf("first attempt: tf=%d jf=%d first=%v err=%v", tf, jf, first, err)
	}

	// The task goes back to ready with its backoff elapsed, and is leased again.
	if requeued, err := s.RequeueTaskForRetry(ctx, "t1", a1.ID, now.Add(-time.Minute), now); err != nil || !requeued {
		t.Fatalf("RequeueTaskForRetry: requeued=%v err=%v", requeued, err)
	}
	a2 := leaseTask(t, s, "t1", "w1")
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

// TestRecordTaskFailure_IdempotentPerAttempt pins that because a worker's
// "failed" status message is delivered at-least-once, RecordTaskFailure
// must count exactly once per attempt. The first call closes the running
// attempt and increments both counters; a second call for the SAME attempt (a
// redelivery) finds the attempt already terminal, returns the SAME counts, and
// does NOT re-increment.
func TestRecordTaskFailure_IdempotentPerAttempt(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	att := leaseTask(t, s, "t1", "w1")
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

func TestRequeueTaskForRetry_ResetsAssignment(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	newJob("j1", "f1", "q1").step("s1", "S1", 0).task("t1", "s1", store.TaskStatusReady).submit(t, s)

	att := leaseTask(t, s, "t1", "w1")

	future := now.Add(30 * time.Second)
	if requeued, err := s.RequeueTaskForRetry(ctx, "t1", att.ID, future, now); err != nil || !requeued {
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

func TestRequeueTaskForRetry_ClearsFailureReason(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	// A failure reason on a task that is about to be requeued is a stale one, a
	// state production never builds up on an in-flight task, so it is written
	// at create and the lease leaves it alone: the requeue's clearing is what
	// the test observes.
	newJob("j1", "f1", "q1").step("s1", "S1", 0).
		taskRow(store.Task{ID: "t1", StepID: "s1", Status: store.TaskStatusReady, FailureReason: "boom"}).
		submit(t, s)

	att := leaseTask(t, s, "t1", "w1")
	if got, err := s.GetTask(ctx, "t1"); err != nil || got.FailureReason != "boom" {
		t.Fatalf("leased task = (%+v, %v), want the seeded failure reason kept", got, err)
	}

	if requeued, err := s.RequeueTaskForRetry(ctx, "t1", att.ID, now.Add(30*time.Second), now); err != nil || !requeued {
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

func TestRequeueTaskForRetry_GuardedToInFlight(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")

	cases := []struct {
		status store.TaskStatus
		reason string
	}{
		{store.TaskStatusCanceled, store.FailureReasonCanceledByUser},
		{store.TaskStatusSucceeded, ""},
		{store.TaskStatusReady, ""},
	}
	seed := newJob("j1", "f1", "q1").step("s1", "S1", 0)
	for i, tc := range cases {
		seed.taskRow(store.Task{ID: "t" + string(rune('1'+i)), StepID: "s1", Status: tc.status, FailureReason: tc.reason})
	}
	seed.submit(t, s)

	if requeued, err := s.RequeueTaskForRetry(ctx, "missing", "no-attempt", now.Add(time.Second), now); err != nil || requeued {
		t.Fatalf("missing task: requeued=%v err=%v, want false,nil", requeued, err)
	}

	for i, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			id := "t" + string(rune('1'+i))

			requeued, err := s.RequeueTaskForRetry(ctx, id, "no-attempt", now.Add(time.Second), now)
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

func TestParkJob_SkipsTerminal(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").as(store.JobStatusRunning).submit(t, s)
	newJob("j2", "f1", "q1").as(store.JobStatusFailed).submit(t, s)

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
	if err := s.ParkJob(ctx, "j2", "x", now); err != nil {
		t.Fatalf("park terminal (expected no-op, no error): %v", err)
	}
	got, err = s.GetJob(ctx, "j2")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusFailed || got.ParkReason != "" {
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
	att := leaseTask(t, s, "t1", "w1")
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

func TestResumeJob_ManualPause_KeepsCounter(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	att := leaseTask(t, s, "t1", "w1")
	if _, _, _, err := s.RecordTaskFailure(ctx, att.ID, "t1", nil, "", "", now); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if err := s.PauseJob(ctx, "j1", now); err != nil {
		t.Fatalf("PauseJob: %v", err)
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
	newJob("j1", "f1", "q1").as(store.JobStatusRunning).submit(t, s)

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

// TestRetryTasks_UnparksAutoParkedJob asserts that a manual retry of an
// AUTO-PARKED job (paused with a park_reason — not terminal) resets the job to
// pending and clears its failure counter and park reason, exactly as it does
// for a terminal job.
func TestRetryTasks_UnparksAutoParkedJob(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	att := leaseTask(t, s, "t1", "w1")
	if _, _, _, err := s.RecordTaskFailure(ctx, att.ID, "t1", nil, "", "", now); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	// The tripping task went terminal-failed and the job parked.
	completeAttempt(t, s, att, store.TaskStatusFailed, store.AttemptStatusFailed)
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

func TestRetryTasks_LeavesManualPauseAlone(t *testing.T) {
	s, ctx, now := recordFailureFixture(t)

	completeAttempt(t, s, leaseTask(t, s, "t1", "w1"), store.TaskStatusFailed, store.AttemptStatusFailed)
	if err := s.PauseJob(ctx, "j1", now); err != nil {
		t.Fatalf("PauseJob: %v", err)
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

func TestFailureReasonSummary_Mixed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").step("s1", "S1", 0).
		taskRow(store.Task{ID: "t0", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "staging"}).
		taskRow(store.Task{ID: "t1", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "staging"}).
		taskRow(store.Task{ID: "t2", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "timeout"}).
		submit(t, s)

	sum, err := s.FailureReasonSummary(ctx, "j1")
	if err != nil {
		t.Fatalf("FailureReasonSummary: %v", err)
	}
	if sum.FailedCount != 3 || sum.DominantReason != "staging" || sum.DistinctReasons != 2 {
		t.Fatalf("got %+v, want {FailedCount:3 DominantReason:staging DistinctReasons:2}", sum)
	}
}

func TestFailureReasonSummary_Tie(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").step("s1", "S1", 0).
		taskRow(store.Task{ID: "t0", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "timeout"}).
		taskRow(store.Task{ID: "t1", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "staging"}).
		submit(t, s)

	sum, err := s.FailureReasonSummary(ctx, "j1")
	if err != nil {
		t.Fatalf("FailureReasonSummary: %v", err)
	}
	if sum.FailedCount != 2 || sum.DominantReason != "staging" || sum.DistinctReasons != 2 {
		t.Fatalf("got %+v, want {FailedCount:2 DominantReason:staging DistinctReasons:2}", sum)
	}
}

func TestFailureReasonSummary_Empty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").step("s1", "S1", 0).task("t0", "s1", store.TaskStatusSucceeded).submit(t, s)

	sum, err := s.FailureReasonSummary(ctx, "j1")
	if err != nil {
		t.Fatalf("FailureReasonSummary: %v", err)
	}
	if (sum != store.FailureSummary{}) {
		t.Fatalf("got %+v, want zero value", sum)
	}
}

// TestCompleteTaskAttempt_ConcurrentReportsHaveOneWinner is the reason the
// terminal move is a compare-and-set. Two goroutines report the same running
// attempt's task as succeeded and as failed; exactly one report may be applied
// and the other must be rejected, never both applied. It carries over the race
// the per-row status writer's test pinned, which the report path superseded.
func TestCompleteTaskAttempt_ConcurrentReportsHaveOneWinner(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	newJob("j1", "f1", "q1").as(store.JobStatusRunning).
		stepAs("s1", "S1", 0, store.StepStatusReady).
		task("t1", "s1", store.TaskStatusReady).
		submit(t, s)
	att := runTask(t, s, "t1", "w1")

	targets := []struct {
		task    store.TaskStatus
		attempt store.AttemptStatus
	}{
		{store.TaskStatusSucceeded, store.AttemptStatusSucceeded},
		{store.TaskStatusFailed, store.AttemptStatusFailed},
	}
	results := make([]store.CompletionResult, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	wg.Add(len(targets))
	for i, target := range targets {
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.CompleteTaskAttempt(ctx, store.AttemptCompletion{
				AttemptID: att.ID, TaskID: "t1", TaskStatus: target.task,
				AttemptStatus: target.attempt, EndedAt: time.Now().UTC(),
			})
		}()
	}
	wg.Wait()

	winner := store.TaskStatus("")
	for i, res := range results {
		switch {
		case errs[i] != nil:
			t.Fatalf("CompleteTaskAttempt(%s): unexpected error: %v", targets[i].task, errs[i])
		case res.Applied && res.Rejected:
			t.Fatalf("CompleteTaskAttempt(%s) = %+v, want exactly one of applied or rejected", targets[i].task, res)
		case res.Applied:
			if winner != "" {
				t.Fatalf("both terminal reports were applied (%s and %s), want exactly one", winner, targets[i].task)
			}
			winner = targets[i].task
		case !res.Rejected:
			t.Fatalf("CompleteTaskAttempt(%s) = %+v, want applied or rejected", targets[i].task, res)
		}
	}
	if winner == "" {
		t.Fatal("no terminal report was applied, want exactly one")
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != winner {
		t.Errorf("task status = %q, want the winning report's %q", got.Status, winner)
	}
}
