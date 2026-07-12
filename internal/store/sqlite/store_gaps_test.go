// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

// Gap tests for item 9 — methods not covered by store_test.go.
//
// Adds tests for:
//   - task_attempt.go: CreateTaskAttempt, GetTaskAttempt, LatestTaskAttempt,
//     UpdateTaskAttempt, CancelJobAttempts, TerminateWorkerAttempts, ListTaskAttempts
//   - usage.go (claim side): CreateClaim, ReleaseClaim,
//     ActiveClaimCount, TryClaimSlots, ReleaseAttemptClaims,
//     ReleaseJobClaims
//   - task_log.go: CreateTaskLog, ListTaskLogs (offset pagination)
//   - audit.go: AppendAuditEntry, ListAuditEntries
//   - job.go: UpdateJob, CancelJobStatus
//   - queue.go: ListQueues

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// insertAttempt creates a running TaskAttempt for the given task and returns it.
func insertAttempt(t *testing.T, s *sqlite.Store, taskID, workerID string, num int) store.TaskAttempt {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	a, err := s.CreateTaskAttempt(context.Background(), store.TaskAttempt{
		ID:            uuid.NewString(),
		TaskID:        taskID,
		WorkerID:      workerID,
		AttemptNumber: num,
		Status:        store.AttemptStatusRunning,
		StartedAt:     now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	return a
}

// ── TaskAttempt ───────────────────────────────────────────────────────────────

func TestTaskAttempt_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	now := time.Now().UTC().Truncate(time.Millisecond)
	attempt, err := s.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:            "a1",
		TaskID:        "t1",
		WorkerID:      "w1",
		AttemptNumber: 1,
		Status:        store.AttemptStatusRunning,
		StartedAt:     now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	if attempt.ID != "a1" {
		t.Errorf("ID: got %q", attempt.ID)
	}

	got, err := s.GetTaskAttempt(ctx, "a1")
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if got.TaskID != "t1" {
		t.Errorf("TaskID: got %q", got.TaskID)
	}
	if got.Status != store.AttemptStatusRunning {
		t.Errorf("Status: got %q", got.Status)
	}
}

func TestTaskAttempt_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetTaskAttempt(context.Background(), "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestTaskAttempt_LatestTaskAttempt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	// Insert two attempts; latest should be AttemptNumber=2.
	insertAttempt(t, s, "t1", "w1", 1)
	a2 := insertAttempt(t, s, "t1", "w1", 2)

	latest, err := s.LatestTaskAttempt(ctx, "t1")
	if err != nil {
		t.Fatalf("LatestTaskAttempt: %v", err)
	}
	if latest.ID != a2.ID {
		t.Errorf("latest ID: got %q, want %q", latest.ID, a2.ID)
	}
	if latest.AttemptNumber != 2 {
		t.Errorf("AttemptNumber: got %d, want 2", latest.AttemptNumber)
	}
}

func TestTaskAttempt_LatestTaskAttempt_NotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.LatestTaskAttempt(context.Background(), "no-such-task")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestTaskAttempt_UpdateTaskAttempt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	a := insertAttempt(t, s, "t1", "w1", 1)

	now := time.Now().UTC().Truncate(time.Millisecond)
	exitCode := 0
	a.Status = store.AttemptStatusSucceeded
	a.EndedAt = &now
	a.ExitCode = &exitCode
	a.SessionID = "session-xyz"

	updated, err := s.UpdateTaskAttempt(ctx, a)
	if err != nil {
		t.Fatalf("UpdateTaskAttempt: %v", err)
	}
	if updated.Status != store.AttemptStatusSucceeded {
		t.Errorf("Status: got %q", updated.Status)
	}
	if updated.EndedAt == nil {
		t.Error("EndedAt: should not be nil after update")
	}
	if updated.ExitCode == nil || *updated.ExitCode != 0 {
		t.Errorf("ExitCode: got %v", updated.ExitCode)
	}
	if updated.SessionID != "session-xyz" {
		t.Errorf("SessionID: got %q", updated.SessionID)
	}
}

func TestTaskAttempt_ListTaskAttempts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	insertAttempt(t, s, "t1", "w1", 1)
	insertAttempt(t, s, "t1", "w1", 2)

	attempts, err := s.ListTaskAttempts(ctx, "t1")
	if err != nil {
		t.Fatalf("ListTaskAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(attempts))
	}
	// Should be ordered AttemptNumber ascending.
	if attempts[0].AttemptNumber != 1 || attempts[1].AttemptNumber != 2 {
		t.Errorf("order wrong: got [%d, %d]", attempts[0].AttemptNumber, attempts[1].AttemptNumber)
	}
}

func TestTaskAttempt_CancelJobAttempts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	insertTask(t, s, "t2", "j1", "s1")

	insertAttempt(t, s, "t1", "w1", 1)
	insertAttempt(t, s, "t2", "w1", 1)

	now := time.Now().UTC()
	n, err := s.CancelJobAttempts(ctx, "j1", now)
	if err != nil {
		t.Fatalf("CancelJobAttempts: %v", err)
	}
	if n != 2 {
		t.Errorf("canceled: got %d, want 2", n)
	}

	// Both attempts should be canceled with EndedAt set.
	a1, err := s.LatestTaskAttempt(ctx, "t1")
	if err != nil {
		t.Fatalf("LatestTaskAttempt t1: %v", err)
	}
	if a1.Status != store.AttemptStatusCanceled {
		t.Errorf("t1 attempt status: got %q, want canceled", a1.Status)
	}
	if a1.EndedAt == nil {
		t.Error("t1 EndedAt should be set after cancel")
	}
}

func TestTaskAttempt_TerminateWorkerAttempts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertWorker(t, s, "w2", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	insertTask(t, s, "t2", "j1", "s1")

	// TerminateWorkerAttempts looks for tasks with assigned_worker_id = workerID
	// and status IN ('assigned','running'), so we must assign the tasks first.
	if err := s.AssignTask(ctx, "t1", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask t1: %v", err)
	}
	if err := s.AssignTask(ctx, "t2", "w2", time.Now()); err != nil {
		t.Fatalf("AssignTask t2: %v", err)
	}

	insertAttempt(t, s, "t1", "w1", 1) // will be terminated
	insertAttempt(t, s, "t2", "w2", 1) // different worker — untouched

	now := time.Now().UTC()
	n, err := s.TerminateWorkerAttempts(ctx, "w1", store.AttemptStatusFailed, now)
	if err != nil {
		t.Fatalf("TerminateWorkerAttempts: %v", err)
	}
	if n != 1 {
		t.Errorf("terminated: got %d, want 1", n)
	}

	a1, err := s.LatestTaskAttempt(ctx, "t1")
	if err != nil {
		t.Fatalf("LatestTaskAttempt t1: %v", err)
	}
	if a1.Status != store.AttemptStatusFailed {
		t.Errorf("status: got %q, want failed", a1.Status)
	}

	// t2's attempt (on w2) should be unchanged.
	a2, err := s.LatestTaskAttempt(ctx, "t2")
	if err != nil {
		t.Fatalf("LatestTaskAttempt t2: %v", err)
	}
	if a2.Status != store.AttemptStatusRunning {
		t.Errorf("t2 status should still be running, got %q", a2.Status)
	}
}

// ── Usage claims ──────────────────────────────────────────────────────────────

func TestUsage_CreateAndReleaseClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	pool, err := s.CreateUsagePool(ctx, store.UsagePool{
		ID:            "p1",
		Name:          "arnold",
		MaxConcurrent: 5,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	a := insertAttempt(t, s, "t1", "w1", 1)

	now := time.Now().UTC().Truncate(time.Millisecond)
	claim, err := s.CreateClaim(ctx, store.UsageClaim{
		ID:            uuid.NewString(),
		PoolID:        pool.ID,
		TaskAttemptID: a.ID,
		ClaimedAt:     now,
	})
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	if claim.PoolID != pool.ID {
		t.Errorf("PoolID: got %q", claim.PoolID)
	}

	// Active count should now be 1.
	n, err := s.ActiveClaimCount(ctx, pool.ID)
	if err != nil {
		t.Fatalf("ActiveClaimCount: %v", err)
	}
	if n != 1 {
		t.Errorf("active count: got %d, want 1", n)
	}

	// Release the claim.
	if err := s.ReleaseClaim(ctx, claim.ID, time.Now().UTC()); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}

	// Active count should now be 0.
	n, err = s.ActiveClaimCount(ctx, pool.ID)
	if err != nil {
		t.Fatalf("ActiveClaimCount after release: %v", err)
	}
	if n != 0 {
		t.Errorf("active count after release: got %d, want 0", n)
	}
}

func TestUsage_TryClaimSlots_UnderCapacity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	pool, err := s.CreateUsagePool(ctx, store.UsagePool{
		ID: "p1", Name: "maya", MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	a := insertAttempt(t, s, "t1", "w1", 1)

	claims := []store.UsagePoolClaim{{
		ClaimID:       uuid.NewString(),
		PoolID:        pool.ID,
		PoolName:      pool.Name,
		MaxConcurrent: pool.MaxConcurrent,
	}}
	if err := s.TryClaimSlots(ctx, a.ID, claims, time.Now().UTC()); err != nil {
		t.Fatalf("TryClaimSlots: %v", err)
	}

	n, err := s.ActiveClaimCount(ctx, pool.ID)
	if err != nil {
		t.Fatalf("ActiveClaimCount: %v", err)
	}
	if n != 1 {
		t.Errorf("active claims: got %d, want 1", n)
	}
}

func TestUsage_TryClaimSlots_AtCapacity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	insertTask(t, s, "t2", "j1", "s1")

	pool, err := s.CreateUsagePool(ctx, store.UsagePool{
		ID: "p1", Name: "nuke", MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	a1 := insertAttempt(t, s, "t1", "w1", 1)
	a2 := insertAttempt(t, s, "t2", "w1", 1)

	claim := []store.UsagePoolClaim{{
		ClaimID:       uuid.NewString(),
		PoolID:        pool.ID,
		PoolName:      pool.Name,
		MaxConcurrent: pool.MaxConcurrent,
	}}
	// First claim succeeds.
	if err := s.TryClaimSlots(ctx, a1.ID, claim, time.Now().UTC()); err != nil {
		t.Fatalf("first TryClaimSlots: %v", err)
	}

	// Second claim should fail with ErrUsageAtCapacity.
	claim[0].ClaimID = uuid.NewString()
	err = s.TryClaimSlots(ctx, a2.ID, claim, time.Now().UTC())
	if !errors.Is(err, store.ErrUsageAtCapacity) {
		t.Errorf("expected ErrUsageAtCapacity, got %v", err)
	}
}

func TestUsage_ReleaseAttemptClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	pool, err := s.CreateUsagePool(ctx, store.UsagePool{
		ID: "p1", Name: "houdini", MaxConcurrent: 5,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	a := insertAttempt(t, s, "t1", "w1", 1)
	claim := []store.UsagePoolClaim{{
		ClaimID:       uuid.NewString(),
		PoolID:        pool.ID,
		PoolName:      pool.Name,
		MaxConcurrent: pool.MaxConcurrent,
	}}
	if err := s.TryClaimSlots(ctx, a.ID, claim, time.Now().UTC()); err != nil {
		t.Fatalf("TryClaimSlots: %v", err)
	}

	n, err := s.ReleaseAttemptClaims(ctx, a.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReleaseAttemptClaims: %v", err)
	}
	if n != 1 {
		t.Errorf("released: got %d, want 1", n)
	}

	count, err := s.ActiveClaimCount(ctx, pool.ID)
	if err != nil {
		t.Fatalf("ActiveClaimCount: %v", err)
	}
	if count != 0 {
		t.Errorf("active after release: got %d, want 0", count)
	}
}

func TestUsage_ReleaseJobClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	insertTask(t, s, "t2", "j1", "s1")

	pool, err := s.CreateUsagePool(ctx, store.UsagePool{
		ID: "p1", Name: "vray", MaxConcurrent: 10,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	a1 := insertAttempt(t, s, "t1", "w1", 1)
	a2 := insertAttempt(t, s, "t2", "w1", 1)

	for _, aID := range []string{a1.ID, a2.ID} {
		claim := []store.UsagePoolClaim{{
			ClaimID:       uuid.NewString(),
			PoolID:        pool.ID,
			PoolName:      pool.Name,
			MaxConcurrent: pool.MaxConcurrent,
		}}
		if err := s.TryClaimSlots(ctx, aID, claim, time.Now().UTC()); err != nil {
			t.Fatalf("TryClaimSlots: %v", err)
		}
	}

	n, err := s.ReleaseJobClaims(ctx, "j1", time.Now().UTC())
	if err != nil {
		t.Fatalf("ReleaseJobClaims: %v", err)
	}
	if n != 2 {
		t.Errorf("released: got %d, want 2", n)
	}
}

func TestUsage_ListUsagePoolUtilization(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	insertTask(t, s, "t2", "j1", "s1")

	// "arnold": 5 seats, will have 2 active + 1 released → in use 2.
	arnold, err := s.CreateUsagePool(ctx, store.UsagePool{
		ID: "p-arnold", Name: "arnold", MaxConcurrent: 5,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool arnold: %v", err)
	}
	// "maya": 3 seats, no claims → in use 0.
	if _, err := s.CreateUsagePool(ctx, store.UsagePool{
		ID: "p-maya", Name: "maya", MaxConcurrent: 3,
	}); err != nil {
		t.Fatalf("CreateUsagePool maya: %v", err)
	}

	a1 := insertAttempt(t, s, "t1", "w1", 1)
	a2 := insertAttempt(t, s, "t2", "w1", 1)

	mkClaim := func() []store.UsagePoolClaim {
		return []store.UsagePoolClaim{{
			ClaimID: uuid.NewString(), PoolID: arnold.ID,
			PoolName: arnold.Name, MaxConcurrent: arnold.MaxConcurrent,
		}}
	}
	if err := s.TryClaimSlots(ctx, a1.ID, mkClaim(), time.Now().UTC()); err != nil {
		t.Fatalf("claim a1: %v", err)
	}
	if err := s.TryClaimSlots(ctx, a2.ID, mkClaim(), time.Now().UTC()); err != nil {
		t.Fatalf("claim a2: %v", err)
	}
	// Release a3's claim so it does not count toward in-use.
	insertTask(t, s, "t3", "j1", "s1")
	a3 := insertAttempt(t, s, "t3", "w1", 1)
	if err := s.TryClaimSlots(ctx, a3.ID, mkClaim(), time.Now().UTC()); err != nil {
		t.Fatalf("claim a3: %v", err)
	}
	if _, err := s.ReleaseAttemptClaims(ctx, a3.ID, time.Now().UTC()); err != nil {
		t.Fatalf("release a3: %v", err)
	}

	usage, err := s.ListUsagePoolUtilization(ctx)
	if err != nil {
		t.Fatalf("ListUsagePoolUtilization: %v", err)
	}
	if len(usage) != 2 {
		t.Fatalf("usage len: got %d, want 2", len(usage))
	}
	// Ordered by name: arnold, maya.
	if usage[0].Name != "arnold" || usage[0].InUse != 2 {
		t.Errorf("arnold: got name=%q in_use=%d, want arnold/2", usage[0].Name, usage[0].InUse)
	}
	if usage[1].Name != "maya" || usage[1].InUse != 0 {
		t.Errorf("maya: got name=%q in_use=%d, want maya/0", usage[1].Name, usage[1].InUse)
	}
}

// ── TaskLog ───────────────────────────────────────────────────────────────────

func TestTaskLog_CreateAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	a := insertAttempt(t, s, "t1", "w1", 1)

	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := range 3 {
		_, err := s.CreateTaskLog(ctx, store.TaskLog{
			ID:         uuid.NewString(),
			TaskID:     "t1",
			AttemptID:  a.ID,
			SeqNum:     int64(i + 1),
			NATSSeq:    int64(i + 1),
			Stream:     store.LogStreamStdout,
			Data:       "line",
			At:         now,
			ReceivedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateTaskLog %d: %v", i, err)
		}
	}

	// afterNATSSeq=0 returns all 3.
	logs, err := s.ListTaskLogs(ctx, a.ID, 0, 10)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("want 3 logs, got %d", len(logs))
	}
	if logs[0].NATSSeq != 1 || logs[2].NATSSeq != 3 {
		t.Errorf("order wrong: seqs %d %d %d", logs[0].NATSSeq, logs[1].NATSSeq, logs[2].NATSSeq)
	}
}

func TestTaskLog_ListTaskLogs_OffsetPagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	a := insertAttempt(t, s, "t1", "w1", 1)

	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := range 5 {
		if _, err := s.CreateTaskLog(ctx, store.TaskLog{
			ID:         uuid.NewString(),
			TaskID:     "t1",
			AttemptID:  a.ID,
			SeqNum:     int64(i + 1),
			NATSSeq:    int64(i + 1),
			Stream:     store.LogStreamStdout,
			Data:       "line",
			At:         now,
			ReceivedAt: now,
		}); err != nil {
			t.Fatalf("CreateTaskLog %d: %v", i, err)
		}
	}

	// afterNATSSeq=2 should return NATSSeq 3, 4, 5.
	logs, err := s.ListTaskLogs(ctx, a.ID, 2, 10)
	if err != nil {
		t.Fatalf("ListTaskLogs offset: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("want 3 logs after NATSSeq=2, got %d", len(logs))
	}
	if logs[0].NATSSeq != 3 {
		t.Errorf("first log NATSSeq: got %d, want 3", logs[0].NATSSeq)
	}

	// limit=2 should cap at 2 results.
	capped, err := s.ListTaskLogs(ctx, a.ID, 0, 2)
	if err != nil {
		t.Fatalf("ListTaskLogs capped: %v", err)
	}
	if len(capped) != 2 {
		t.Errorf("capped: got %d, want 2", len(capped))
	}
}

func TestTaskLog_StderrStream(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	a := insertAttempt(t, s, "t1", "w1", 1)

	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := s.CreateTaskLog(ctx, store.TaskLog{
		ID:         uuid.NewString(),
		TaskID:     "t1",
		AttemptID:  a.ID,
		SeqNum:     1,
		NATSSeq:    1,
		Stream:     store.LogStreamStderr,
		Data:       "error line",
		At:         now,
		ReceivedAt: now,
	}); err != nil {
		t.Fatalf("CreateTaskLog stderr: %v", err)
	}

	logs, err := s.ListTaskLogs(ctx, a.ID, 0, 10)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 log, got %d", len(logs))
	}
	if logs[0].Stream != store.LogStreamStderr {
		t.Errorf("Stream: got %q, want stderr", logs[0].Stream)
	}
}

// ── Audit ─────────────────────────────────────────────────────────────────────

func TestAudit_AppendAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	entries := []store.AuditEntry{
		{
			ID:         uuid.NewString(),
			EntityType: "job",
			EntityID:   "j1",
			Action:     "submitted",
			Actor:      "alice",
			Details:    map[string]any{"priority": 75},
			CreatedAt:  now,
		},
		{
			ID:         uuid.NewString(),
			EntityType: "job",
			EntityID:   "j1",
			Action:     "canceled",
			Actor:      "alice",
			Details:    map[string]any{},
			CreatedAt:  now.Add(time.Second),
		},
		{
			ID:         uuid.NewString(),
			EntityType: "worker",
			EntityID:   "w1",
			Action:     "registered",
			Actor:      "",
			Details:    map[string]any{},
			CreatedAt:  now,
		},
	}

	for _, e := range entries {
		if err := s.AppendAuditEntry(ctx, e); err != nil {
			t.Fatalf("AppendAuditEntry(%q): %v", e.Action, err)
		}
	}

	// Filter by job j1 — should return 2.
	jobEntries, err := s.ListAuditEntries(ctx, "job", "j1")
	if err != nil {
		t.Fatalf("ListAuditEntries job/j1: %v", err)
	}
	if len(jobEntries) != 2 {
		t.Fatalf("want 2 job entries, got %d", len(jobEntries))
	}
	// Ordered by CreatedAt ascending.
	if jobEntries[0].Action != "submitted" {
		t.Errorf("first action: got %q, want submitted", jobEntries[0].Action)
	}
	if jobEntries[1].Action != "canceled" {
		t.Errorf("second action: got %q, want canceled", jobEntries[1].Action)
	}
	// Details should round-trip.
	if jobEntries[0].Details["priority"] == nil {
		t.Error("Details[priority] should not be nil")
	}

	// Filter by worker w1.
	workerEntries, err := s.ListAuditEntries(ctx, "worker", "w1")
	if err != nil {
		t.Fatalf("ListAuditEntries worker/w1: %v", err)
	}
	if len(workerEntries) != 1 {
		t.Fatalf("want 1 worker entry, got %d", len(workerEntries))
	}

	// List all (empty type and ID).
	all, err := s.ListAuditEntries(ctx, "", "")
	if err != nil {
		t.Fatalf("ListAuditEntries all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 total entries, got %d", len(all))
	}
}

// ── Job: UpdateJob and CancelJobStatus ───────────────────────────────────────

func TestJob_UpdateJob(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	j := insertJob(t, s, "j1", "f1", "q1")

	j.Priority = 99
	j.Owner = "bob"
	updated, err := s.UpdateJob(ctx, j)
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if updated.Priority != 99 {
		t.Errorf("Priority: got %d, want 99", updated.Priority)
	}
	if updated.Owner != "bob" {
		t.Errorf("Owner: got %q, want bob", updated.Owner)
	}

	fetched, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if fetched.Priority != 99 {
		t.Errorf("fetched Priority: got %d", fetched.Priority)
	}
}

// TestJob_UpdateJob_RetryPolicy verifies that UpdateJob persists the per-job
// retry-policy overrides (max_attempts, retry_delay_seconds, failure_limit)
// added in Task 8, and that it never touches failed_attempts or park_reason —
// those are lifecycle-managed by the scheduler.
func TestJob_UpdateJob_RetryPolicy(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	j := insertJob(t, s, "j1", "f1", "q1")

	maxAttempts, delay, limit := 7, 30, 40
	j.MaxAttempts = &maxAttempts
	j.RetryDelaySeconds = &delay
	j.FailureLimit = &limit
	// These are lifecycle-managed and must not be written by UpdateJob even
	// when the caller's struct carries non-zero values (e.g. re-fetched then
	// mutated only for the fields above).
	j.FailedAttempts = 3
	j.ParkReason = "should not persist"

	updated, err := s.UpdateJob(ctx, j)
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if updated.MaxAttempts == nil || *updated.MaxAttempts != 7 {
		t.Errorf("MaxAttempts: got %v, want 7", updated.MaxAttempts)
	}
	if updated.RetryDelaySeconds == nil || *updated.RetryDelaySeconds != 30 {
		t.Errorf("RetryDelaySeconds: got %v, want 30", updated.RetryDelaySeconds)
	}
	if updated.FailureLimit == nil || *updated.FailureLimit != 40 {
		t.Errorf("FailureLimit: got %v, want 40", updated.FailureLimit)
	}

	fetched, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if fetched.MaxAttempts == nil || *fetched.MaxAttempts != 7 {
		t.Errorf("fetched MaxAttempts: got %v, want 7", fetched.MaxAttempts)
	}
	if fetched.RetryDelaySeconds == nil || *fetched.RetryDelaySeconds != 30 {
		t.Errorf("fetched RetryDelaySeconds: got %v, want 30", fetched.RetryDelaySeconds)
	}
	if fetched.FailureLimit == nil || *fetched.FailureLimit != 40 {
		t.Errorf("fetched FailureLimit: got %v, want 40", fetched.FailureLimit)
	}
	if fetched.FailedAttempts != 0 {
		t.Errorf("fetched FailedAttempts: got %d, want 0 (UpdateJob must not write it)", fetched.FailedAttempts)
	}
	if fetched.ParkReason != "" {
		t.Errorf("fetched ParkReason: got %q, want empty (UpdateJob must not write it)", fetched.ParkReason)
	}
}

func TestJob_CancelJobStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")

	// Transition to running first (so cancel has something to do).
	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusRunning); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}

	if err := s.CancelJobStatus(ctx, "j1"); err != nil {
		t.Fatalf("CancelJobStatus: %v", err)
	}

	j, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if j.Status != store.JobStatusCanceled {
		t.Errorf("Status: got %q, want canceled", j.Status)
	}
}

func TestJob_CancelJobStatus_AlreadyTerminal_Conflict(t *testing.T) {
	// CancelJobStatus on a completed job should return ErrConflict.
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")

	if err := s.UpdateJobStatus(ctx, "j1", store.JobStatusCompleted); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}

	err := s.CancelJobStatus(ctx, "j1")
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict for already-completed job, got %v", err)
	}
}

// ── Queue: ListQueues ─────────────────────────────────────────────────────────

func TestQueue_ListQueues(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertFarm(t, s, "f2", "F2")
	insertQueue(t, s, "q1", "f1", "Alpha")
	insertQueue(t, s, "q2", "f1", "Beta")
	insertQueue(t, s, "q3", "f2", "Gamma")

	// Filter by farm f1.
	page, err := s.ListQueues(ctx, store.ListQueuesOptions{FarmID: "f1"})
	if err != nil {
		t.Fatalf("ListQueues f1: %v", err)
	}
	if page.Total != 2 {
		t.Errorf("f1 total: got %d, want 2", page.Total)
	}

	// No filter — all 3.
	all, err := s.ListQueues(ctx, store.ListQueuesOptions{})
	if err != nil {
		t.Fatalf("ListQueues all: %v", err)
	}
	if all.Total != 3 {
		t.Errorf("all total: got %d, want 3", all.Total)
	}

	// Pagination.
	paged, err := s.ListQueues(ctx, store.ListQueuesOptions{
		Pagination: store.Pagination{Limit: 2, Offset: 0},
	})
	if err != nil {
		t.Fatalf("ListQueues paged: %v", err)
	}
	if len(paged.Items) != 2 {
		t.Errorf("paged items: got %d, want 2", len(paged.Items))
	}
}
