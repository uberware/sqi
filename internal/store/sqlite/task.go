// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const taskCols = `
	id, job_id, step_id, name, parameters, status,
	assigned_worker_id, assigned_at, created_at, updated_at`

const (
	sqlInsertTask = `
INSERT INTO tasks (
	id, job_id, step_id, name, parameters, status,
	assigned_worker_id, assigned_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + taskCols

	sqlGetTask = `SELECT ` + taskCols + ` FROM tasks WHERE id = ?`

	sqlUpdateTaskStatus = `
UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`

	sqlAssignTask = `
UPDATE tasks
SET assigned_worker_id = ?, assigned_at = ?, status = 'assigned', updated_at = ?
WHERE id = ?`

	// Joins to jobs and queues to apply priority ordering and skip paused queues.
	sqlListReadyTasks = `
SELECT t.` + taskCols + `
FROM tasks t
JOIN jobs  j ON t.job_id    = j.id
JOIN queues q ON j.queue_id = q.id
WHERE t.status = 'ready'
  AND j.farm_id = ?
  AND q.paused  = 0
ORDER BY j.priority DESC, t.created_at ASC
LIMIT ?`

	sqlReclaimWorkerTasks = `
UPDATE tasks
SET status = 'ready', assigned_worker_id = NULL, assigned_at = NULL, updated_at = ?
WHERE assigned_worker_id = ?
  AND status IN ('assigned', 'running')`
)

func scanTask(row scanner) (store.Task, error) {
	var t store.Task
	var paramsJSON, status string
	var assignedWorkerID, assignedAt sql.NullString
	var createdAt, updatedAt string

	if err := row.Scan(
		&t.ID, &t.JobID, &t.StepID, &t.Name, &paramsJSON, &status,
		&assignedWorkerID, &assignedAt, &createdAt, &updatedAt,
	); err != nil {
		return store.Task{}, err
	}

	t.Status = store.TaskStatus(status)
	t.AssignedWorkerID = assignedWorkerID.String
	t.AssignedAt = nullTextToTime(assignedAt)
	t.CreatedAt = mustTime(createdAt)
	t.UpdatedAt = mustTime(updatedAt)

	params, err := unmarshalJSON(paramsJSON, map[string]string{})
	if err != nil {
		return store.Task{}, err
	}
	t.Parameters = params

	return t, nil
}

// CreateTask implements [store.TaskStore].
func (s *Store) CreateTask(ctx context.Context, task store.Task) (store.Task, error) {
	paramsJSON, err := marshalJSON(task.Parameters)
	if err != nil {
		return store.Task{}, err
	}
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertTask.QueryRowContext(ctx,
		task.ID, task.JobID, task.StepID, task.Name, paramsJSON, string(task.Status),
		nullString(task.AssignedWorkerID), nullTimeToText(task.AssignedAt), now, now)
	out, err := scanTask(row)
	return out, mapErr(err)
}

// GetTask implements [store.TaskStore].
func (s *Store) GetTask(ctx context.Context, id string) (store.Task, error) {
	row := s.stmtGetTask.QueryRowContext(ctx, id)
	out, err := scanTask(row)
	return out, mapErr(err)
}

// taskSortColumns maps [store.TaskSortField] values to safe SQL column names.
var taskSortColumns = map[store.TaskSortField]string{
	store.TaskSortByCreatedAt: "created_at",
	store.TaskSortByStatus:    "status",
	store.TaskSortByUpdatedAt: "updated_at",
	store.TaskSortByName:      "name",
}

// ListTasks implements [store.TaskStore].
// Dynamic filters use parameterised ad-hoc queries due to the variable WHERE
// clause. The sort column is looked up from a hard-coded allow-list.
func (s *Store) ListTasks(ctx context.Context, opts store.ListTasksOptions) (store.Page[store.Task], error) {
	opts.Pagination.Validate() //nolint:errcheck // Validate only clamps; never errors

	where := ` WHERE 1=1`
	args := make([]any, 0, 4)

	if opts.JobID != "" {
		where += ` AND job_id = ?`
		args = append(args, opts.JobID)
	}
	if opts.StepID != "" {
		where += ` AND step_id = ?`
		args = append(args, opts.StepID)
	}
	if opts.Status != "" {
		where += ` AND status = ?`
		args = append(args, string(opts.Status))
	}
	if opts.WorkerID != "" {
		where += ` AND assigned_worker_id = ?`
		args = append(args, opts.WorkerID)
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`+where, args...).Scan(&total); err != nil {
		return store.Page[store.Task]{}, mapErr(err)
	}

	col, ok := taskSortColumns[opts.SortBy]
	if !ok {
		col = "created_at"
	}
	dir := sortDirKeyword(opts.SortDir)

	// col comes from taskSortColumns (hard-coded allow-list); dir is "ASC" or
	// "DESC" from sortDirKeyword; where uses only ? placeholders for user values.
	q := `SELECT ` + taskCols + ` FROM tasks` + where + //nolint:gosec // see comment above
		` ORDER BY ` + col + ` ` + dir +
		` LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
	if err != nil {
		return store.Page[store.Task]{}, mapErr(err)
	}
	defer rows.Close()

	tasks := make([]store.Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return store.Page[store.Task]{}, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return store.Page[store.Task]{}, err
	}
	return store.Page[store.Task]{
		Items:  tasks,
		Total:  total,
		Limit:  opts.Pagination.Limit,
		Offset: opts.Pagination.Offset,
	}, nil
}

// UpdateTaskStatus implements [store.TaskStore].
func (s *Store) UpdateTaskStatus(ctx context.Context, id string, status store.TaskStatus) error {
	res, err := s.stmtUpdateTaskStatus.ExecContext(ctx, string(status), timeToText(time.Now().UTC()), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// AssignTask implements [store.TaskStore].
func (s *Store) AssignTask(ctx context.Context, id, workerID string, assignedAt time.Time) error {
	now := timeToText(time.Now().UTC())
	res, err := s.stmtAssignTask.ExecContext(ctx, workerID, timeToText(assignedAt), now, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// ReclaimWorkerTasks implements [store.TaskStore].
func (s *Store) ReclaimWorkerTasks(ctx context.Context, workerID string) (int, error) {
	now := timeToText(time.Now().UTC())
	res, err := s.stmtReclaimWorkerTasks.ExecContext(ctx, now, workerID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListReadyTasks implements [store.TaskStore].
func (s *Store) ListReadyTasks(ctx context.Context, farmID string, limit int) ([]store.Task, error) {
	rows, err := s.stmtListReadyTasks.QueryContext(ctx, farmID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var tasks []store.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
