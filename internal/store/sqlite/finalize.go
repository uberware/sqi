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

	// sqlListStuckSteps selects the steps sqlFinalizeStep would finalize now.
	sqlListStuckSteps = `SELECT ` + stepCols + `
FROM   steps
WHERE  status NOT IN ('completed', 'failed', 'canceled')
  AND  EXISTS     (SELECT 1 FROM tasks t WHERE t.step_id = steps.id)
  AND  NOT EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id
                   AND t.status NOT IN ('succeeded', 'failed', 'canceled'))
ORDER BY job_id, step_order`
)

// FinalizeStep implements [store.StepStore].
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
