// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
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

	// Joins to jobs, queues, and steps to apply the full selection ordering:
	//   1. j.priority DESC        — highest-priority jobs first
	//   2. j.created_at ASC       — earlier-submitted jobs win ties
	//   3. s.step_order ASC       — within a job, earlier steps run first
	//   4. t.created_at ASC       — stable tiebreaker within a step
	//
	// All task columns are fully qualified (t.) to avoid ambiguity now that
	// the steps join introduces columns with overlapping names (name, status,
	// created_at, updated_at, job_id).
	sqlListReadyTasks = `
SELECT t.id, t.job_id, t.step_id, t.name, t.parameters, t.status,
       t.assigned_worker_id, t.assigned_at, t.created_at, t.updated_at
FROM   tasks  t
JOIN   jobs   j ON t.job_id   = j.id
JOIN   queues q ON j.queue_id = q.id
JOIN   steps  s ON t.step_id  = s.id
WHERE  t.status  = 'ready'
  AND  j.farm_id = ?
  AND  q.paused  = 0
ORDER BY j.priority DESC, j.created_at ASC, s.step_order ASC, t.created_at ASC
LIMIT ?`

	sqlReclaimWorkerTasks = `
UPDATE tasks
SET status = 'ready', assigned_worker_id = NULL, assigned_at = NULL, updated_at = ?
WHERE assigned_worker_id = ?
  AND status IN ('assigned', 'running')`

	// Counts tasks in 'assigned' or 'running' state for per-queue policy (task 51).
	// Joins to jobs so we can filter by queue_id.
	sqlCountActiveTasksInQueue = `
SELECT COUNT(*)
FROM   tasks t
JOIN   jobs  j ON t.job_id = j.id
WHERE  j.queue_id = ?
  AND  t.status IN ('assigned', 'running')`

	// Counts tasks in 'assigned' or 'running' state for per-farm policy (task 51).
	sqlCountActiveTasksInFarm = `
SELECT COUNT(*)
FROM   tasks t
JOIN   jobs  j ON t.job_id = j.id
WHERE  j.farm_id = ?
  AND  t.status IN ('assigned', 'running')`

	// Per-queue count of tasks in 'ready' state for a given farm (task 55).
	// Used to populate the sqi_scheduler_queue_depth Prometheus gauge.
	sqlCountReadyTasksByQueue = `
SELECT j.queue_id, COUNT(*)
FROM   tasks t
JOIN   jobs  j ON t.job_id = j.id
WHERE  t.status  = 'ready'
  AND  j.farm_id = ?
GROUP BY j.queue_id`

	// Counts tasks for a given job grouped by status (task 73).
	// Used by the REST layer to include aggregate task counts in job detail responses.
	sqlCountTasksByJob = `
SELECT status, COUNT(*)
FROM   tasks
WHERE  job_id = ?
GROUP BY status`

	// Cancels all non-terminal tasks for a job and returns the number of rows
	// updated (task 54). The caller first SELECTs active tasks within the same
	// transaction to capture worker IDs before this UPDATE clears them.
	sqlCancelJobTasks = `
UPDATE tasks
SET    status = 'canceled', assigned_worker_id = NULL, assigned_at = NULL, updated_at = ?
WHERE  job_id = ?
  AND  status IN ('pending', 'ready', 'assigned', 'running')`
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

// CountActiveTasksInQueue implements [store.TaskStore].
func (s *Store) CountActiveTasksInQueue(ctx context.Context, queueID string) (int, error) {
	var n int
	err := s.stmtCountActiveTasksInQueue.QueryRowContext(ctx, queueID).Scan(&n)
	return n, mapErr(err)
}

// CountActiveTasksInFarm implements [store.TaskStore].
func (s *Store) CountActiveTasksInFarm(ctx context.Context, farmID string) (int, error) {
	var n int
	err := s.stmtCountActiveTasksInFarm.QueryRowContext(ctx, farmID).Scan(&n)
	return n, mapErr(err)
}

// CountReadyTasksByQueue implements [store.TaskStore].
// Returns a map of queue ID → ready-task count for the given farm.
// Queues with zero ready tasks are omitted.
func (s *Store) CountReadyTasksByQueue(ctx context.Context, farmID string) (map[string]int, error) {
	rows, err := s.stmtCountReadyTasksByQueue.QueryContext(ctx, farmID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var queueID string
		var n int
		if err := rows.Scan(&queueID, &n); err != nil {
			return nil, err
		}
		counts[queueID] = n
	}
	return counts, rows.Err()
}

// CancelJobTasks implements [store.TaskStore].
//
// The SELECT and UPDATE execute inside a single SQLite transaction so no
// concurrent scheduler tick can assign a task between observation and
// cancellation.  The rows cursor is closed inside a helper closure before the
// UPDATE runs, which avoids any potential cursor/write contention on the
// single-connection pool.
func (s *Store) CancelJobTasks(ctx context.Context, jobID string, now time.Time) ([]store.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin tx for cancel job tasks: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is best-effort after commit

	// Capture tasks that are currently assigned or running so the scheduler can
	// publish cancel signals to their workers.  We read into a slice before the
	// UPDATE so the cursor is closed by the time we write.
	active, err := func() ([]store.Task, error) {
		rows, queryErr := tx.QueryContext(ctx, `
SELECT id, job_id, step_id, name, parameters, status,
       assigned_worker_id, assigned_at, created_at, updated_at
FROM   tasks
WHERE  job_id = ?
  AND  status IN ('assigned', 'running')`, jobID)
		if queryErr != nil {
			return nil, fmt.Errorf("sqlite: select active tasks for job %s: %w", jobID, mapErr(queryErr))
		}
		defer rows.Close()

		var tasks []store.Task
		for rows.Next() {
			t, scanErr := scanTask(rows)
			if scanErr != nil {
				return nil, scanErr
			}
			tasks = append(tasks, t)
		}
		return tasks, rows.Err()
	}()
	if err != nil {
		return nil, err
	}

	// Transition all non-terminal tasks to canceled, clearing the worker
	// assignment so stale heartbeat messages cannot re-assign them.
	if _, err = tx.ExecContext(ctx, sqlCancelJobTasks, timeToText(now), jobID); err != nil {
		return nil, fmt.Errorf("sqlite: cancel tasks for job %s: %w", jobID, mapErr(err))
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit cancel job tasks: %w", err)
	}
	return active, nil
}

// CountTasksByJob implements [store.TaskStore].
// Returns the number of tasks for the given job keyed by status.
// Statuses with zero tasks are omitted from the returned map.
func (s *Store) CountTasksByJob(ctx context.Context, jobID string) (map[store.TaskStatus]int, error) {
	rows, err := s.stmtCountTasksByJob.QueryContext(ctx, jobID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	counts := make(map[store.TaskStatus]int)
	for rows.Next() {
		var status store.TaskStatus
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[status] = n
	}
	return counts, rows.Err()
}
