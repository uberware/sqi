// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"

	"github.com/uberware/sqi/internal/store"
)

// stepCols lists every column returned by step queries.
// host_requirements is stored as a JSON object (see [store.StepHostRequirements]).
// compute_location is a plain TEXT mirror of the computelocation attribute for
// SQL-level pre-filtering in the scheduler assignment loop.
const stepCols = `
	id, job_id, name, depends_on, step_order, status,
	host_requirements, compute_location, created_at, updated_at`

const (
	sqlInsertStep = `
INSERT INTO steps (
	id, job_id, name, depends_on, step_order, status,
	host_requirements, compute_location, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + stepCols

	sqlGetStep = `SELECT ` + stepCols + ` FROM steps WHERE id = ?`

	sqlListSteps = `SELECT ` + stepCols + `
FROM steps WHERE job_id = ?
ORDER BY step_order ASC`
)

func scanStep(row scanner) (store.Step, error) {
	var step store.Step
	var dependsOnJSON, hostReqJSON, status, createdAt, updatedAt string

	if err := row.Scan(
		&step.ID, &step.JobID, &step.Name, &dependsOnJSON,
		&step.StepOrder, &status,
		&hostReqJSON, &step.ComputeLocation,
		&createdAt, &updatedAt,
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

	// host_requirements is stored as JSON; "null" → nil pointer.
	hr, err := unmarshalJSON(hostReqJSON, (*store.StepHostRequirements)(nil))
	if err != nil {
		return store.Step{}, err
	}
	step.HostRequirements = hr

	return step, nil
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
