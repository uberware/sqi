// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const stepCols = `id, job_id, name, depends_on, step_order, status, created_at, updated_at`

const (
	sqlInsertStep = `
INSERT INTO steps (id, job_id, name, depends_on, step_order, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + stepCols

	sqlGetStep = `SELECT ` + stepCols + ` FROM steps WHERE id = ?`

	sqlListSteps = `SELECT ` + stepCols + `
FROM steps WHERE job_id = ?
ORDER BY step_order ASC`

	sqlUpdateStepStatus = `
UPDATE steps SET status = ?, updated_at = ? WHERE id = ?`
)

func scanStep(row scanner) (store.Step, error) {
	var step store.Step
	var dependsOnJSON, status, createdAt, updatedAt string

	if err := row.Scan(
		&step.ID, &step.JobID, &step.Name, &dependsOnJSON,
		&step.StepOrder, &status, &createdAt, &updatedAt,
	); err != nil {
		return store.Step{}, err
	}

	step.Status = store.StepStatus(status)
	step.CreatedAt = mustTime(createdAt)
	step.UpdatedAt = mustTime(updatedAt)

	deps, err := unmarshalJSON(dependsOnJSON, []string{})
	if err != nil {
		return store.Step{}, err
	}
	step.DependsOn = deps

	return step, nil
}

// CreateStep implements [store.StepStore].
func (s *Store) CreateStep(ctx context.Context, step store.Step) (store.Step, error) {
	dependsOnJSON, err := marshalJSON(step.DependsOn)
	if err != nil {
		return store.Step{}, err
	}
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertStep.QueryRowContext(ctx,
		step.ID, step.JobID, step.Name, dependsOnJSON,
		step.StepOrder, string(step.Status), now, now)
	out, err := scanStep(row)
	return out, mapErr(err)
}

// GetStep implements [store.StepStore].
func (s *Store) GetStep(ctx context.Context, id string) (store.Step, error) {
	row := s.stmtGetStep.QueryRowContext(ctx, id)
	out, err := scanStep(row)
	return out, mapErr(err)
}

// ListSteps implements [store.StepStore].
func (s *Store) ListSteps(ctx context.Context, jobID string) ([]store.Step, error) {
	rows, err := s.stmtListSteps.QueryContext(ctx, jobID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var steps []store.Step
	for rows.Next() {
		step, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, rows.Err()
}

// UpdateStepStatus implements [store.StepStore].
func (s *Store) UpdateStepStatus(ctx context.Context, id string, status store.StepStatus) error {
	res, err := s.stmtUpdateStepStatus.ExecContext(ctx, string(status), timeToText(time.Now().UTC()), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}
