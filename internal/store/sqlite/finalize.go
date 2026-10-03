// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	// sqlFinalizeStep is invariant I4 for steps: the outcome is computed from
	// the tasks inside the statement that writes it, so a retry that revives a
	// task between a caller's read and this write is seen here.
	sqlFinalizeStep = `
UPDATE steps
SET    status = CASE
         WHEN EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id AND t.status = 'failed')   THEN 'failed'
         WHEN EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id AND t.status = 'canceled') THEN 'canceled'
         ELSE 'completed' END,
       updated_at = ?
WHERE  id = ?
  AND  status NOT IN ('completed', 'failed', 'canceled')
  AND  NOT EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id
                   AND t.status NOT IN ('succeeded', 'failed', 'canceled'))
RETURNING status`

	// sqlFinalizeJob is invariant I4 for jobs, computed over the job's steps.
	sqlFinalizeJob = `
UPDATE jobs
SET    status = CASE
         WHEN EXISTS (SELECT 1 FROM steps s WHERE s.job_id = jobs.id AND s.status = 'failed')   THEN 'failed'
         WHEN EXISTS (SELECT 1 FROM steps s WHERE s.job_id = jobs.id AND s.status = 'canceled') THEN 'canceled'
         ELSE 'completed' END,
       completed_at = ?,
       updated_at   = ?
WHERE  id = ?
  AND  status NOT IN ('completed', 'failed', 'canceled')
  AND  NOT EXISTS (SELECT 1 FROM steps s WHERE s.job_id = jobs.id
                   AND s.status NOT IN ('completed', 'failed', 'canceled'))
RETURNING status`

	// sqlListStuckSteps selects the steps sqlFinalizeStep would finalize now,
	// restricted to steps of a job that is not itself terminal. The job
	// condition is what keeps a healthy farm quiet: canceling a job writes its
	// tasks and its job row but never its steps, so a canceled job's open steps
	// look stuck by their tasks alone, and every start would rewrite those of
	// each job canceled since the start before it. A terminal job has no
	// downstream that needs its steps finalized, and its cross-job dependents
	// follow the job's own status.
	sqlListStuckSteps = `SELECT ` + stepCols + `
FROM   steps
WHERE  status NOT IN ('completed', 'failed', 'canceled')
  AND  EXISTS     (SELECT 1 FROM jobs j WHERE j.id = steps.job_id
                   AND j.status NOT IN ('completed', 'failed', 'canceled'))
  AND  EXISTS     (SELECT 1 FROM tasks t WHERE t.step_id = steps.id)
  AND  NOT EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id
                   AND t.status NOT IN ('succeeded', 'failed', 'canceled'))
ORDER BY job_id, step_order`
)

// FinalizeStep implements [store.StepStore].
//
// Anchor: the step's job row. The step's current status is read before the
// anchor and is used only to report an already-terminal step when the guarded
// UPDATE writes nothing. On Postgres a finalize committing between that read
// and the UPDATE makes it stale (the step is reported in flight rather than
// terminal), so H4c should re-read it under the lock.
func (s *Store) FinalizeStep(ctx context.Context, id string, now time.Time) (store.StepStatus, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("sqlite: begin finalize step: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	var jobID, current string
	if err := tx.QueryRowContext(ctx, `SELECT job_id, status FROM steps WHERE id = ?`, id).Scan(&jobID, &current); err != nil {
		return "", false, mapErr(err)
	}
	if err := lockAnchors(ctx, tx, jobAnchor(jobID)); err != nil {
		return "", false, err
	}
	var status string
	err = tx.QueryRowContext(ctx, sqlFinalizeStep, timeToText(now.UTC()), id).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not finalized now: either a task is still in flight, or the step was
		// already terminal before this call.
		if isTerminalStep(store.StepStatus(current)) {
			return store.StepStatus(current), false, tx.Commit()
		}
		return "", false, tx.Commit()
	case err != nil:
		return "", false, fmt.Errorf("sqlite: finalize step %s: %w", id, mapErr(err))
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("sqlite: commit finalize step: %w", mapErr(err))
	}
	return store.StepStatus(status), true, nil
}

// FinalizeJob implements [store.JobStore].
//
// Anchor: the job row. As in [Store.FinalizeStep], the current status is read
// before the anchor, only to report an already-terminal job, and H4c should
// re-read it under the lock.
func (s *Store) FinalizeJob(ctx context.Context, id string, now time.Time) (store.JobStatus, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("sqlite: begin finalize job: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, id).Scan(&current); err != nil {
		return "", false, mapErr(err)
	}
	if err := lockAnchors(ctx, tx, jobAnchor(id)); err != nil {
		return "", false, err
	}
	nowText := timeToText(now.UTC())
	var status string
	err = tx.QueryRowContext(ctx, sqlFinalizeJob, nowText, nowText, id).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if store.JobStatus(current).IsTerminal() {
			return store.JobStatus(current), false, tx.Commit()
		}
		return "", false, tx.Commit()
	case err != nil:
		return "", false, fmt.Errorf("sqlite: finalize job %s: %w", id, mapErr(err))
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("sqlite: commit finalize job: %w", mapErr(err))
	}
	return store.JobStatus(status), true, nil
}

// ListStuckSteps implements [store.StepStore].
func (s *Store) ListStuckSteps(ctx context.Context) ([]store.Step, error) {
	rows, err := s.rdb.QueryContext(ctx, sqlListStuckSteps)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list stuck steps: %w", mapErr(err))
	}
	defer rows.Close()
	var out []store.Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func isTerminalStep(s store.StepStatus) bool {
	return s == store.StepStatusCompleted || s == store.StepStatusFailed || s == store.StepStatusCanceled
}

// sqlGuardStepPending is the I1 guard for releasing or canceling a step: it
// writes only a step that is still pending, so a step another writer already
// moved (finalized, canceled by a job cancel) is never overwritten. The task
// half of the move is [sqlTransitionStepPendingTasks], shared with
// [Store.TransitionStepPendingTasks] so the two cannot drift.
const sqlGuardStepPending = `
UPDATE steps SET status = ?, updated_at = ? WHERE id = ? AND status = 'pending'`

// ReleaseStep implements [store.StepStore].
func (s *Store) ReleaseStep(ctx context.Context, id string, now time.Time) (bool, []store.Task, error) {
	return s.movePendingStep(ctx, id, store.StepStatusReady, store.TaskStatusReady, "", now)
}

// CancelPendingStep implements [store.StepStore].
func (s *Store) CancelPendingStep(ctx context.Context, id, reason string, now time.Time) (bool, []store.Task, error) {
	return s.movePendingStep(ctx, id, store.StepStatusCanceled, store.TaskStatusCanceled, reason, now)
}

// movePendingStep moves a pending step and its pending tasks together, in one
// transaction. The anchor is the job row (the step's parent), taken before the
// step is written so a concurrent finalize or cancel of the same job is
// ordered against this move. The step row is written first and guards the
// task move: when the step is no longer pending nothing is written at all.
func (s *Store) movePendingStep(
	ctx context.Context, id string, stepTo store.StepStatus, taskTo store.TaskStatus, reason string, now time.Time,
) (bool, []store.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("sqlite: begin move pending step: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	var jobID string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM steps WHERE id = ?`, id).Scan(&jobID); err != nil {
		return false, nil, mapErr(err)
	}
	if err := lockAnchors(ctx, tx, jobAnchor(jobID)); err != nil {
		return false, nil, err
	}
	nowText := timeToText(now.UTC())
	res, err := tx.ExecContext(ctx, sqlGuardStepPending, string(stepTo), nowText, id)
	if err != nil {
		return false, nil, fmt.Errorf("sqlite: move pending step %s: %w", id, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("sqlite: move pending step %s: %w", id, mapErr(err))
	}
	if n == 0 {
		return false, nil, nil
	}
	tasks, err := queryTasksTx(ctx, tx, sqlTransitionStepPendingTasks, string(taskTo), nowText, reason, id)
	if err != nil {
		return false, nil, fmt.Errorf("sqlite: move pending tasks of step %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("sqlite: commit move pending step: %w", mapErr(err))
	}
	return true, tasks, nil
}

// queryTasksTx runs a task-returning statement inside tx and scans every row.
func queryTasksTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]store.Task, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, mapErr(rows.Err())
}

const (
	// sqlReleaseBlockedJob is invariant I4 for cross-job dependencies: the
	// "every upstream completed" check is part of the statement that writes the
	// release, and the status guard (I1) means a job another writer already
	// canceled or released is never rewritten. An edge whose upstream no longer
	// exists counts as unsatisfied (the edge deliberately survives the
	// upstream's deletion, see migration 00020).
	sqlReleaseBlockedJob = `
UPDATE jobs SET status = 'pending', updated_at = ?
WHERE  id = ? AND status = 'blocked'
  AND  NOT EXISTS (
         SELECT 1 FROM job_dependencies jd
         LEFT JOIN jobs u ON u.id = jd.depends_on_job_id
         WHERE jd.job_id = jobs.id AND (u.id IS NULL OR u.status != 'completed'))`

	sqlCancelBlockedJobRow = `
UPDATE jobs SET status = 'canceled', completed_at = ?, updated_at = ?
WHERE  id = ? AND status = 'blocked'`

	sqlCancelJobOpenSteps = `
UPDATE steps SET status = 'canceled', updated_at = ?
WHERE  job_id = ? AND status NOT IN ('completed', 'failed', 'canceled')`

	// sqlCancelJobPendingTasks is the job-wide twin of
	// [sqlTransitionStepPendingTasks]: same columns, same reason-stamping rule.
	sqlCancelJobPendingTasks = `
UPDATE tasks
SET    status = 'canceled', updated_at = ?, unschedulable_reason = '',
       failure_reason = CASE WHEN failure_reason = '' THEN ? ELSE failure_reason END
WHERE  job_id = ? AND status = 'pending'
RETURNING ` + taskCols
)

// ReleaseBlockedJob implements [store.JobStore].
//
// Anchors: the job row, then each upstream job row (H4c locks those FOR
// SHARE, so a concurrent finalize of an upstream is ordered against this
// release). The write is one guarded UPDATE; see [sqlReleaseBlockedJob].
func (s *Store) ReleaseBlockedJob(ctx context.Context, id string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("sqlite: begin release blocked job: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	anchors, err := blockedJobAnchorsTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if err := lockAnchors(ctx, tx, anchors...); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, sqlReleaseBlockedJob, timeToText(now.UTC()), id)
	if err != nil {
		return false, fmt.Errorf("sqlite: release blocked job %s: %w", id, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: release blocked job %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("sqlite: commit release blocked job: %w", mapErr(err))
	}
	return n == 1, nil
}

// CancelBlockedJob implements [store.JobStore].
//
// Anchors as for [Store.ReleaseBlockedJob]. The job row is written first and
// guards the rest: when the job is no longer blocked nothing is written at
// all, otherwise its open steps and pending tasks follow in the same
// transaction so a canceled job is never observed with live children.
func (s *Store) CancelBlockedJob(ctx context.Context, id, reason string, now time.Time) (bool, []store.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("sqlite: begin cancel blocked job: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	anchors, err := blockedJobAnchorsTx(ctx, tx, id)
	if err != nil {
		return false, nil, err
	}
	if err := lockAnchors(ctx, tx, anchors...); err != nil {
		return false, nil, err
	}
	nowText := timeToText(now.UTC())
	res, err := tx.ExecContext(ctx, sqlCancelBlockedJobRow, nowText, nowText, id)
	if err != nil {
		return false, nil, fmt.Errorf("sqlite: cancel blocked job %s: %w", id, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("sqlite: cancel blocked job %s: %w", id, err)
	}
	if n == 0 {
		return false, nil, nil
	}
	if _, err := tx.ExecContext(ctx, sqlCancelJobOpenSteps, nowText, id); err != nil {
		return false, nil, fmt.Errorf("sqlite: cancel steps of blocked job %s: %w", id, mapErr(err))
	}
	tasks, err := queryTasksTx(ctx, tx, sqlCancelJobPendingTasks, nowText, reason, id)
	if err != nil {
		return false, nil, fmt.Errorf("sqlite: cancel tasks of blocked job %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("sqlite: commit cancel blocked job: %w", mapErr(err))
	}
	return true, tasks, nil
}

// blockedJobAnchorsTx returns the anchor list for a blocked-job operation: the
// job row, then each upstream job row in id order. It returns ErrNotFound for
// an unknown job.
func blockedJobAnchorsTx(ctx context.Context, tx *sql.Tx, id string) ([]anchor, error) {
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id = ?`, id).Scan(&one); err != nil {
		return nil, mapErr(err)
	}
	rows, err := tx.QueryContext(ctx, sqlListJobDependencyIDs, id)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	anchors := []anchor{jobAnchor(id)}
	for rows.Next() {
		var up string
		if err := rows.Scan(&up); err != nil {
			return nil, err
		}
		anchors = append(anchors, jobAnchor(up))
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return anchors, nil
}

const (
	// sqlPromoteJobRunning is the I1 guard for the first start: only a pending
	// job is promoted, so a pause, cancel or completion that landed first is
	// never overwritten. started_at is kept when already set.
	sqlPromoteJobRunning = `
UPDATE jobs SET status = 'running', started_at = COALESCE(started_at, ?), updated_at = ?
WHERE id = ? AND status = 'pending'`

	// sqlPauseJob is the I1 guard for an administrative pause: only a pending
	// or running job is paused.
	sqlPauseJob = `
UPDATE jobs SET status = 'paused', updated_at = ?
WHERE id = ? AND status IN ('pending', 'running')`
)

// PromoteJobRunning implements [store.JobStore]. It is one guarded UPDATE, so
// it needs no anchor: an unknown job and a job that is not pending both write
// nothing and return false.
func (s *Store) PromoteJobRunning(ctx context.Context, id string, now time.Time) (bool, error) {
	nowText := timeToText(now.UTC())
	res, err := s.db.ExecContext(ctx, sqlPromoteJobRunning, nowText, nowText, id)
	if err != nil {
		return false, fmt.Errorf("sqlite: promote job %s running: %w", id, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: promote job %s running: %w", id, mapErr(err))
	}
	return n == 1, nil
}

// PauseJob implements [store.JobStore]. The guarded UPDATE does the work; a
// zero-row result is ambiguous between an unknown job and one in another
// status, and a follow-up read tells them apart.
func (s *Store) PauseJob(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, sqlPauseJob, timeToText(now.UTC()), id)
	if err != nil {
		return fmt.Errorf("sqlite: pause job %s: %w", id, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: pause job %s: %w", id, mapErr(err))
	}
	if n == 1 {
		return nil
	}
	if _, err := s.GetJob(ctx, id); err != nil {
		return err // ErrNotFound for an unknown job
	}
	return store.ErrConflict
}
