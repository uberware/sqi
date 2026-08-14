// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// openTestStoreWB opens a SQLite store for white-box tests that need access to
// unexported fields (e.g. st.db). The store is closed via t.Cleanup; callers
// must not close it themselves.
//
// File-backed, not ":memory:", for the reason openTestStore documents: only a
// file-backed store gets the separate read pool.
func openTestStoreWB(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir()+"/test.db", DefaultOptions())
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

// seedTerminalJobAt creates a job, drives it to the given terminal status, then
// backdates both completed_at and updated_at to ts so the COALESCE cutoff
// comparison is deterministic.
func seedTerminalJobAt(t *testing.T, st *Store, id string, status store.JobStatus, ts time.Time) {
	t.Helper()
	ctx := context.Background()
	seedJob(t, st, id)
	if err := st.UpdateJobStatus(ctx, id, status); err != nil {
		t.Fatalf("seedTerminalJobAt UpdateJobStatus(%q, %v): %v", id, status, err)
	}
	if _, err := st.db.ExecContext(
		ctx,
		`UPDATE jobs SET completed_at = ?, updated_at = ? WHERE id = ?`,
		timeToText(ts.UTC()), timeToText(ts.UTC()), id,
	); err != nil {
		t.Fatalf("seedTerminalJobAt backdate(%q): %v", id, err)
	}
}

// deletedJobIDs collects the IDs from a []store.DeletedJob slice, sorts them,
// and returns them so callers can compare against a pre-sorted want slice.
func deletedJobIDs(deleted []store.DeletedJob) []string {
	ids := make([]string, len(deleted))
	for i, d := range deleted {
		ids[i] = d.ID
	}
	slices.Sort(ids)
	return ids
}

func TestStore_DeleteTerminalJobsBefore(t *testing.T) {
	ctx := context.Background()

	cutoff := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Hour)

	tests := []struct {
		name          string
		includeFailed bool
		wantDeleted   []string // job IDs expected removed (sorted)
	}{
		{name: "excludes failed by default", includeFailed: false, wantDeleted: []string{"canceled-old", "completed-old"}},
		{name: "includes failed when set", includeFailed: true, wantDeleted: []string{"canceled-old", "completed-old", "failed-old"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openTestStoreWB(t)
			// Seed: <status>-old at `old`, plus a recent completed and a running job that must survive.
			seedTerminalJobAt(t, st, "completed-old", store.JobStatusCompleted, old)
			seedTerminalJobAt(t, st, "canceled-old", store.JobStatusCanceled, old)
			seedTerminalJobAt(t, st, "failed-old", store.JobStatusFailed, old)
			seedTerminalJobAt(t, st, "completed-recent", store.JobStatusCompleted, recent)
			seedJob(t, st, "running-active") // status pending; never terminal

			got, err := st.DeleteTerminalJobsBefore(ctx, cutoff, tt.includeFailed)
			if err != nil {
				t.Fatalf("DeleteTerminalJobsBefore: %v", err)
			}
			gotIDs := deletedJobIDs(got)
			if !slices.Equal(gotIDs, tt.wantDeleted) {
				t.Fatalf("deleted = %v, want %v", gotIDs, tt.wantDeleted)
			}
			// Survivors still present.
			for _, id := range []string{"completed-recent", "running-active"} {
				if _, err := st.GetJob(ctx, id); err != nil {
					t.Fatalf("GetJob(%s): %v", id, err)
				}
			}
		})
	}
}

// TestStore_DeleteTerminalJobsBefore_KeepsUpstreamNeededByBlockedDependent
// verifies the retention-purge fix: a completed upstream older than the
// cutoff must NOT be purged while a dependent still waits on it (blocked),
// because the reconciler would read the purge as a missing upstream and
// wrongly cancel the dependent even though the upstream actually succeeded.
// Once the dependent reaches a terminal status, the upstream becomes
// eligible for purge again.
func TestStore_DeleteTerminalJobsBefore_KeepsUpstreamNeededByBlockedDependent(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreWB(t)

	cutoff := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)

	seedTerminalJobAt(t, st, "upstream-old", store.JobStatusCompleted, old)
	dependent := seedJob(t, st, "dependent-blocked")
	if err := st.CreateJobDependencies(ctx, dependent.ID, []string{"upstream-old"}); err != nil {
		t.Fatalf("CreateJobDependencies: %v", err)
	}
	if err := st.UpdateJobStatus(ctx, dependent.ID, store.JobStatusBlocked); err != nil {
		t.Fatalf("UpdateJobStatus(blocked): %v", err)
	}

	got, err := st.DeleteTerminalJobsBefore(ctx, cutoff, false)
	if err != nil {
		t.Fatalf("DeleteTerminalJobsBefore: %v", err)
	}
	if ids := deletedJobIDs(got); len(ids) != 0 {
		t.Fatalf("deleted = %v, want none (upstream still needed by blocked dependent)", ids)
	}
	if _, err := st.GetJob(ctx, "upstream-old"); err != nil {
		t.Fatalf("GetJob(upstream-old) after guarded sweep: %v", err)
	}

	// Once the dependent is terminal, the upstream is no longer protected.
	if err := st.UpdateJobStatus(ctx, dependent.ID, store.JobStatusCanceled); err != nil {
		t.Fatalf("UpdateJobStatus(canceled): %v", err)
	}
	got, err = st.DeleteTerminalJobsBefore(ctx, cutoff, false)
	if err != nil {
		t.Fatalf("DeleteTerminalJobsBefore (2nd sweep): %v", err)
	}
	if ids := deletedJobIDs(got); !slices.Equal(ids, []string{"upstream-old"}) {
		t.Fatalf("deleted = %v, want [upstream-old]", ids)
	}
	if _, err := st.GetJob(ctx, "upstream-old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetJob(upstream-old) after unblock sweep = %v, want ErrNotFound", err)
	}
}

// TestJob_BlockedStatusAndDependencyTable verifies the Task 1 deliverables in
// isolation, before Task 2 adds CreateJobDependencies/ListJobDependencyIDs:
// a job created with JobStatusBlocked round-trips through CreateJob/GetJob,
// and the job_dependencies table exists and accepts rows shaped as designed.
func TestJob_BlockedStatusAndDependencyTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStoreWB(t)

	upstream := seedJob(t, st, "upstream")

	if _, err := st.CreateFarm(ctx, store.Farm{ID: "blocked-farm", Name: "blocked-farm"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "blocked-queue", FarmID: "blocked-farm", Name: "blocked-queue"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	created, err := st.CreateJob(ctx, store.Job{
		ID:             "blocked",
		FarmID:         "blocked-farm",
		QueueID:        "blocked-queue",
		Name:           "comp",
		Priority:       50,
		Status:         store.JobStatusBlocked,
		RawTemplate:    "{}",
		TemplateFormat: store.TemplateFormatJSON,
	})
	if err != nil {
		t.Fatalf("CreateJob(blocked): %v", err)
	}
	if created.Status != store.JobStatusBlocked {
		t.Fatalf("CreateJob status = %q, want blocked", created.Status)
	}

	got, err := st.GetJob(ctx, "blocked")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.JobStatusBlocked {
		t.Fatalf("GetJob status = %q, want blocked", got.Status)
	}

	// job_dependencies table exists (created by the 00020 migration).
	var name string
	if err := st.db.QueryRowContext(
		ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='job_dependencies'`,
	).Scan(&name); err != nil {
		t.Fatalf("job_dependencies table missing: %v", err)
	}
	if name != "job_dependencies" {
		t.Fatalf("unexpected sqlite_master name %q", name)
	}

	// A raw edge insert round-trips with the designed shape (job_id,
	// depends_on_job_id, created_at), confirming the schema Task 2's
	// CreateJobDependencies/ListJobDependencyIDs will build on.
	if _, err := st.db.ExecContext(
		ctx,
		`INSERT INTO job_dependencies (job_id, depends_on_job_id, created_at) VALUES (?, ?, ?)`,
		"blocked", upstream.ID, time.Now().UTC(),
	); err != nil {
		t.Fatalf("raw insert into job_dependencies: %v", err)
	}

	var depID string
	if err := st.db.QueryRowContext(
		ctx,
		`SELECT depends_on_job_id FROM job_dependencies WHERE job_id = ?`, "blocked",
	).Scan(&depID); err != nil {
		t.Fatalf("query job_dependencies: %v", err)
	}
	if depID != upstream.ID {
		t.Fatalf("depends_on_job_id = %q, want %q", depID, upstream.ID)
	}
}

// TestJob_ListDependentsAndBlocked exercises CreateJobDependencies,
// ListDependents, ListBlockedJobs, and GetJob's DependsOn population together.
func TestJob_ListDependentsAndBlocked(t *testing.T) {
	// Not t.Parallel(): sqlite.Open calls goose.SetBaseFS/SetDialect, which
	// mutate package-level goose state; running concurrently with another
	// parallel Open (e.g. TestJob_BlockedStatusAndDependencyTable in this
	// same file) trips the race detector on that pre-existing global state.
	ctx := context.Background()
	st := openTestStoreWB(t)

	up := seedJob(t, st, "up-listdeps")
	down := store.Job{
		ID: "down-listdeps", FarmID: up.FarmID, QueueID: up.QueueID, Name: "down",
		Priority: 50, Status: store.JobStatusBlocked, RawTemplate: "{}",
		TemplateFormat: store.TemplateFormatJSON,
	}
	if _, err := st.CreateJob(ctx, down); err != nil {
		t.Fatalf("CreateJob(down): %v", err)
	}
	if err := st.CreateJobDependencies(ctx, down.ID, []string{up.ID}); err != nil {
		t.Fatalf("CreateJobDependencies: %v", err)
	}

	deps, err := st.ListDependents(ctx, up.ID)
	if err != nil {
		t.Fatalf("ListDependents: %v", err)
	}
	if len(deps) != 1 || deps[0] != down.ID {
		t.Fatalf("ListDependents = %v, want [%s]", deps, down.ID)
	}

	blocked, err := st.ListBlockedJobs(ctx)
	if err != nil {
		t.Fatalf("ListBlockedJobs: %v", err)
	}
	if len(blocked) != 1 || blocked[0].ID != down.ID {
		t.Fatalf("ListBlockedJobs = %v, want [%s]", blocked, down.ID)
	}

	// GetJob populates DependsOn.
	got, err := st.GetJob(ctx, down.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != up.ID {
		t.Fatalf("GetJob DependsOn = %v, want [%s]", got.DependsOn, up.ID)
	}
}

// TestJob_DeleteJob_RemovesOutgoingEdgesKeepsIncoming verifies the asymmetric
// delete behavior: deleting the downstream job removes its outgoing edges,
// but deleting the upstream job leaves the (now-dangling) edge intact so the
// reconciler can observe "upstream deleted".
func TestJob_DeleteJob_RemovesOutgoingEdgesKeepsIncoming(t *testing.T) {
	// Not t.Parallel(): see comment in TestJob_ListDependentsAndBlocked.
	ctx := context.Background()
	st := openTestStoreWB(t)

	up := seedJob(t, st, "up-delcascade")
	down := store.Job{
		ID: "down-delcascade", FarmID: up.FarmID, QueueID: up.QueueID, Name: "down",
		Priority: 50, Status: store.JobStatusBlocked, RawTemplate: "{}",
		TemplateFormat: store.TemplateFormatJSON,
	}
	if _, err := st.CreateJob(ctx, down); err != nil {
		t.Fatalf("CreateJob(down): %v", err)
	}
	if err := st.CreateJobDependencies(ctx, down.ID, []string{up.ID}); err != nil {
		t.Fatalf("CreateJobDependencies: %v", err)
	}

	// Deleting the UPSTREAM must NOT delete the edge (no FK on depends_on_job_id):
	if err := st.DeleteJob(ctx, up.ID); err != nil {
		t.Fatalf("DeleteJob(up): %v", err)
	}
	deps, err := st.ListJobDependencyIDs(ctx, down.ID)
	if err != nil {
		t.Fatalf("ListJobDependencyIDs: %v", err)
	}
	if len(deps) != 1 || deps[0] != up.ID {
		t.Fatalf("after upstream delete, edge should survive: got %v", deps)
	}

	// Deleting the DOWNSTREAM removes its outgoing edges (job_id cascade):
	if err := st.DeleteJob(ctx, down.ID); err != nil {
		t.Fatalf("DeleteJob(down): %v", err)
	}
	deps, err = st.ListDependents(ctx, up.ID)
	if err != nil {
		t.Fatalf("ListDependents: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("after downstream delete, no edges should remain: got %v", deps)
	}
}

// TestJob_CreateJobDependencies_Dedup verifies that calling
// CreateJobDependencies twice with an overlapping upstream ID does not
// produce a duplicate edge — INSERT OR IGNORE plus the (job_id,
// depends_on_job_id) primary key make the operation idempotent.
func TestJob_CreateJobDependencies_Dedup(t *testing.T) {
	// Not t.Parallel(): see comment in TestJob_ListDependentsAndBlocked.
	ctx := context.Background()
	st := openTestStoreWB(t)

	up1 := seedJob(t, st, "up1-dedup")
	up2 := seedJob(t, st, "up2-dedup")
	down := store.Job{
		ID: "down-dedup", FarmID: up1.FarmID, QueueID: up1.QueueID, Name: "down",
		Priority: 50, Status: store.JobStatusBlocked, RawTemplate: "{}",
		TemplateFormat: store.TemplateFormatJSON,
	}
	if _, err := st.CreateJob(ctx, down); err != nil {
		t.Fatalf("CreateJob(down): %v", err)
	}

	if err := st.CreateJobDependencies(ctx, down.ID, []string{up1.ID}); err != nil {
		t.Fatalf("CreateJobDependencies (first call): %v", err)
	}
	// Second call repeats up1 (already recorded) and adds up2.
	if err := st.CreateJobDependencies(ctx, down.ID, []string{up1.ID, up2.ID}); err != nil {
		t.Fatalf("CreateJobDependencies (second call): %v", err)
	}

	deps, err := st.ListJobDependencyIDs(ctx, down.ID)
	if err != nil {
		t.Fatalf("ListJobDependencyIDs: %v", err)
	}
	want := []string{up1.ID, up2.ID}
	slices.Sort(want)
	if !slices.Equal(deps, want) {
		t.Fatalf("ListJobDependencyIDs = %v, want %v (no duplicate edge)", deps, want)
	}
}

// TestJob_ListJobDependencyIDs_OrderedByUpstreamID locks in the ordering fix:
// ListJobDependencyIDs must return upstream IDs sorted by ID, regardless of
// the order they were passed to CreateJobDependencies or created in.
func TestJob_ListJobDependencyIDs_OrderedByUpstreamID(t *testing.T) {
	// Not t.Parallel(): see comment in TestJob_ListDependentsAndBlocked.
	ctx := context.Background()
	st := openTestStoreWB(t)

	zeta := seedJob(t, st, "zeta-order")
	alpha := seedJob(t, st, "alpha-order")
	mike := seedJob(t, st, "mike-order")
	down := store.Job{
		ID: "down-order", FarmID: zeta.FarmID, QueueID: zeta.QueueID, Name: "down",
		Priority: 50, Status: store.JobStatusBlocked, RawTemplate: "{}",
		TemplateFormat: store.TemplateFormatJSON,
	}
	if _, err := st.CreateJob(ctx, down); err != nil {
		t.Fatalf("CreateJob(down): %v", err)
	}

	// Deliberately not alphabetical.
	if err := st.CreateJobDependencies(ctx, down.ID, []string{zeta.ID, alpha.ID, mike.ID}); err != nil {
		t.Fatalf("CreateJobDependencies: %v", err)
	}

	deps, err := st.ListJobDependencyIDs(ctx, down.ID)
	if err != nil {
		t.Fatalf("ListJobDependencyIDs: %v", err)
	}
	want := []string{alpha.ID, mike.ID, zeta.ID}
	if !slices.Equal(deps, want) {
		t.Fatalf("ListJobDependencyIDs = %v, want %v", deps, want)
	}
}
