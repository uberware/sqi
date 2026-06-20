// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// openTestStoreWB opens an in-memory SQLite store for white-box tests that
// need access to unexported fields (e.g. st.db). The store is closed via
// t.Cleanup; callers must not close it themselves.
func openTestStoreWB(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:", DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	})
	return s
}

// seedJob creates the minimal set of fixtures (farm → queue → job) needed
// for job-level tests and returns the created job.
func seedJob(t *testing.T, st *Store, id string) store.Job {
	t.Helper()
	ctx := context.Background()

	if _, err := st.CreateFarm(ctx, store.Farm{
		ID: id + "-farm", Name: id + "-farm",
	}); err != nil {
		t.Fatalf("seedJob CreateFarm(%q): %v", id, err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{
		ID:     id + "-queue",
		FarmID: id + "-farm",
		Name:   id + "-queue",
	}); err != nil {
		t.Fatalf("seedJob CreateQueue(%q): %v", id, err)
	}
	j, err := st.CreateJob(ctx, store.Job{
		ID:             id,
		FarmID:         id + "-farm",
		QueueID:        id + "-queue",
		Name:           id,
		Status:         store.JobStatusPending,
		Priority:       50,
		TemplateFormat: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("seedJob CreateJob(%q): %v", id, err)
	}
	return j
}

// seedJobWithChildren seeds a job with a step, task, task attempt, task log,
// and usage claim (all the child rows the cascade must remove). Returns the job.
func seedJobWithChildren(t *testing.T, st *Store, id string) store.Job {
	t.Helper()
	ctx := context.Background()
	j := seedJob(t, st, id)

	if _, err := st.CreateStep(ctx, store.Step{
		ID:        id + "-step",
		JobID:     id,
		Name:      "step",
		StepOrder: 0,
		Status:    store.StepStatusPending,
		DependsOn: []string{},
	}); err != nil {
		t.Fatalf("seedJobWithChildren CreateStep(%q): %v", id, err)
	}

	if _, err := st.CreateTask(ctx, store.Task{
		ID:         id + "-task",
		JobID:      id,
		StepID:     id + "-step",
		Name:       "task",
		Status:     store.TaskStatusPending,
		Parameters: map[string]string{},
	}); err != nil {
		t.Fatalf("seedJobWithChildren CreateTask(%q): %v", id, err)
	}

	// task_attempts.worker_id FK to workers was relaxed in migration 00012, so
	// any non-empty string is accepted without a matching workers row.
	if _, err := st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:            id + "-attempt",
		TaskID:        id + "-task",
		WorkerID:      "worker-stub",
		AttemptNumber: 1,
		Status:        store.AttemptStatusRunning,
		StartedAt:     time.Now().UTC(),
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedJobWithChildren CreateTaskAttempt(%q): %v", id, err)
	}

	if _, err := st.CreateTaskLog(ctx, store.TaskLog{
		ID:        id + "-log",
		TaskID:    id + "-task",
		AttemptID: id + "-attempt",
		SeqNum:    1,
		NATSSeq:   1,
		At:        time.Now().UTC(),
		Stream:    store.LogStreamStdout,
		Data:      "hello",
	}); err != nil {
		t.Fatalf("seedJobWithChildren CreateTaskLog(%q): %v", id, err)
	}

	// usage_claims.pool_id has an FK to usage_pools.id — create the pool first.
	if _, err := st.CreateUsagePool(ctx, store.UsagePool{
		ID:            id + "-pool",
		Name:          id + "-pool",
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatalf("seedJobWithChildren CreateUsagePool(%q): %v", id, err)
	}

	if _, err := st.CreateClaim(ctx, store.UsageClaim{
		ID:            id + "-claim",
		PoolID:        id + "-pool",
		TaskAttemptID: id + "-attempt",
		ClaimedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedJobWithChildren CreateClaim(%q): %v", id, err)
	}

	return j
}

// assertNoRows fails the test if the COUNT(*) query with the given arg returns
// a non-zero result.
func assertNoRows(t *testing.T, st *Store, query, arg string) {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(context.Background(), query, arg).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if n != 0 {
		t.Fatalf("query %q returned %d rows, want 0", query, n)
	}
}

func TestStore_DeleteJob_CascadesAllChildren(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreWB(t)

	// Seed two jobs; only the first is deleted.
	keep := seedJob(t, st, "job-keep")
	del := seedJobWithChildren(t, st, "job-del")

	if err := st.DeleteJob(ctx, del.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	if _, err := st.GetJob(ctx, del.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetJob(deleted) = %v, want ErrNotFound", err)
	}
	// Child rows for the deleted job are gone.
	assertNoRows(t, st, "SELECT COUNT(*) FROM steps WHERE job_id = ?", del.ID)
	assertNoRows(t, st, "SELECT COUNT(*) FROM tasks WHERE job_id = ?", del.ID)
	assertNoRows(t, st, "SELECT COUNT(*) FROM task_attempts WHERE task_id IN (SELECT id FROM tasks WHERE job_id = ?)", del.ID)
	assertNoRows(t, st, "SELECT COUNT(*) FROM task_logs WHERE task_id IN (SELECT id FROM tasks WHERE job_id = ?)", del.ID)
	// The kept job survives.
	if _, err := st.GetJob(ctx, keep.ID); err != nil {
		t.Fatalf("GetJob(keep): %v", err)
	}
}

func TestStore_DeleteJob_NotFound(t *testing.T) {
	st := openTestStoreWB(t)
	if err := st.DeleteJob(context.Background(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteJob(missing) = %v, want ErrNotFound", err)
	}
}
