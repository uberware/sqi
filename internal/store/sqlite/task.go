// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const taskCols = `
	id, job_id, step_id, name, parameters, status,
	assigned_worker_id, assigned_at, created_at, updated_at, required_cores,
	unschedulable_reason, failed_attempts, retry_after, failure_reason`

const (
	sqlInsertTask = `
INSERT INTO tasks (
	id, job_id, step_id, name, parameters, status,
	assigned_worker_id, assigned_at, created_at, updated_at, required_cores,
	unschedulable_reason, failed_attempts, retry_after, failure_reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + taskCols

	sqlGetTask = `SELECT ` + taskCols + ` FROM tasks WHERE id = ?`

	// sqlSelectTaskStatus reads the current status inside the transaction of a
	// compare-and-set ([casTaskStatusTx]), so the state-machine check and the
	// guarded write share one transaction.
	sqlSelectTaskStatus = `
SELECT status FROM tasks WHERE id = ?`

	// sqlSetTaskUnschedulableReason writes only while the task is ready: the
	// sweep reads its candidates before it writes, and a task a lease took in
	// between must not be stamped with a reason that says it cannot be
	// scheduled (F15). The status test is evaluated inside the UPDATE (I1).
	sqlSetTaskUnschedulableReason = `
UPDATE tasks SET unschedulable_reason = ?, updated_at = ? WHERE id = ? AND status = 'ready'`

	sqlSetTaskFailureReason = `
UPDATE tasks SET failure_reason = ?, updated_at = ? WHERE id = ?`

	// sqlSetTaskFailureReasonIfEmpty only writes when no reason is set yet, so a
	// more specific cause already recorded (e.g. a cascade-cancel) survives a
	// later user-cancel. Zero rows updated is a legitimate no-op.
	sqlSetTaskFailureReasonIfEmpty = `
UPDATE tasks SET failure_reason = ?, updated_at = ? WHERE id = ? AND failure_reason = ''`

	sqlAssignTask = `
UPDATE tasks
SET assigned_worker_id = ?, assigned_at = ?, status = 'assigned', updated_at = ?, unschedulable_reason = ''
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
	// sqlListReadyTasks fetches tasks eligible for assignment.
	// farmID = '' means "all farms" (used when the scheduler manages every farm).
	// Two bind parameters are required for farmID: one for the empty check and
	// one for the value filter. A task backing off (retry_after in the
	// future) or belonging to a paused/terminal job is excluded.
	sqlListReadyTasks = `
SELECT t.id, t.job_id, t.step_id, t.name, t.parameters, t.status,
       t.assigned_worker_id, t.assigned_at, t.created_at, t.updated_at, t.required_cores,
       t.unschedulable_reason, t.failed_attempts, t.retry_after, t.failure_reason
FROM   tasks  t
JOIN   jobs   j ON t.job_id   = j.id
JOIN   queues q ON j.queue_id = q.id
JOIN   steps  s ON t.step_id  = s.id
WHERE  (? = '' OR j.farm_id = ?)
  AND  ` + sqlLeasableTask + `
ORDER BY j.priority DESC, j.created_at ASC, s.step_order ASC, t.created_at ASC
LIMIT ?`

	// sqlLeasableTask is the eligibility predicate a task must pass to be
	// leased, over the aliases t (tasks), j (jobs) and q (queues): the task is
	// ready and its backoff has elapsed (its one bind parameter is now), its
	// queue is unpaused, and its job is neither paused nor terminal.
	// sqlListReadyTasks, sqlCountReadyTasksByQueue and sqlLeaseTaskGuarded
	// (lease.go) all embed it, so the lease's guard cannot drift from the list
	// the scheduler chose the task from.
	sqlLeasableTask = `t.status  = 'ready'
  AND  q.paused  = 0
  AND  j.status NOT IN ('paused','completed','failed','canceled')
  AND  (t.retry_after IS NULL OR t.retry_after <= ?)`

	// sqlReclaimTaskSet is the reset every reclaim writes: the task goes back to
	// ready with no worker. Its one bind parameter is updated_at.
	sqlReclaimTaskSet = `
UPDATE tasks
SET    status = 'ready', assigned_worker_id = NULL, assigned_at = NULL, updated_at = ?, unschedulable_reason = ''`

	sqlReclaimWorkerTasks = sqlReclaimTaskSet + `
WHERE assigned_worker_id = ?
  AND status IN ('assigned', 'running')`

	// sqlReclaimStaleAssignedTasks returns tasks stuck in 'assigned' past the
	// cutoff to ready, and RETURNING hands back exactly the rows this statement
	// changed, as they are after the reset (invariant I2). Only 'assigned'
	// (never 'running') so an in-progress task is never disturbed by the timer.
	// Its binds are updated_at, then cutoff.
	sqlReclaimStaleAssignedTasks = sqlReclaimTaskSet + `
WHERE  status = 'assigned' AND assigned_at IS NOT NULL AND assigned_at < ?
RETURNING ` + taskCols

	// Counts tasks in 'assigned' or 'running' state for per-queue policy.
	// Joins to jobs so we can filter by queue_id.
	sqlCountActiveTasksInQueue = `
SELECT COUNT(*)
FROM   tasks t
JOIN   jobs  j ON t.job_id = j.id
WHERE  j.queue_id = ?
  AND  t.status IN ('assigned', 'running')`

	// Counts tasks in 'assigned' or 'running' state for per-farm policy.
	sqlCountActiveTasksInFarm = `
SELECT COUNT(*)
FROM   tasks t
JOIN   jobs  j ON t.job_id = j.id
WHERE  j.farm_id = ?
  AND  t.status IN ('assigned', 'running')`

	// Per-queue count of LEASABLE ready tasks for a given farm — the same
	// eligibility predicate as sqlListReadyTasks (sqlLeasableTask: backoff
	// elapsed, queue unpaused, job schedulable), so the sweep neither reports
	// depth for nor wakes lease waiters on queues whose only ready work is
	// backing off or held under a paused/parked job.
	// Used for the sqi_scheduler_queue_depth Prometheus gauge and the
	// heartbeat-sweep queue wake. farmID = '' means "all farms".
	sqlCountReadyTasksByQueue = `
SELECT j.queue_id, COUNT(*)
FROM   tasks  t
JOIN   jobs   j ON t.job_id   = j.id
JOIN   queues q ON j.queue_id = q.id
WHERE  (? = '' OR j.farm_id = ?)
  AND  ` + sqlLeasableTask + `
GROUP BY j.queue_id`

	// Counts tasks for a given job grouped by status.
	// Used by the REST layer to include aggregate task counts in job detail responses.
	sqlCountTasksByJob = `
SELECT status, COUNT(*)
FROM   tasks
WHERE  job_id = ?
GROUP BY status`

	// Counts ready tasks for a given job that carry a non-empty unschedulable
	// reason. Used by the REST layer to surface an "unschedulable" count
	// alongside the per-status task counts in job responses.
	sqlCountUnschedulableTasksByJob = `
SELECT COUNT(*)
FROM   tasks
WHERE  job_id = ? AND status = 'ready' AND unschedulable_reason <> ''`

	// Groups a job's failed tasks by failure_reason, ordered by frequency
	// descending then reason ascending so the first row is deterministically
	// the dominant reason (see FailureReasonSummary).
	sqlFailureReasonSummary = `
SELECT failure_reason, COUNT(*) AS n
FROM   tasks
WHERE  job_id = ? AND status = 'failed' AND failure_reason != ''
GROUP  BY failure_reason
ORDER  BY n DESC, failure_reason ASC`

	// Cancels all non-terminal tasks for a job. Both [Store.CancelJobExecution]
	// and the [Store.CancelJobTasks] fixture run it, each first SELECTing the
	// active tasks within the same transaction to capture worker IDs before this
	// UPDATE clears them.
	// The reason is stamped only on rows with no failure_reason yet, so a more
	// specific cause recorded earlier (e.g. a cascade-cancel) survives.
	sqlCancelJobTasks = `
UPDATE tasks
SET    status = 'canceled', assigned_worker_id = NULL, assigned_at = NULL, updated_at = ?, unschedulable_reason = '',
       failure_reason = CASE WHEN failure_reason = '' THEN ? ELSE failure_reason END
WHERE  job_id = ?
  AND  status IN ('pending', 'ready', 'assigned', 'running')`

	// Transitions all pending tasks of a step to a new status and returns the
	// affected rows. A pending task has never been assigned, so there is no worker
	// assignment to clear. Single statement — not bounded by MaxLimit.
	// The failure reason (empty when promoting to ready) is stamped only on rows
	// with no failure_reason yet, so a more specific cause survives.
	sqlTransitionStepPendingTasks = `
UPDATE tasks
SET    status = ?, updated_at = ?, unschedulable_reason = '',
       failure_reason = CASE WHEN failure_reason = '' THEN ? ELSE failure_reason END
WHERE  step_id = ? AND status = 'pending'
RETURNING ` + taskCols

	sqlCommittedCores = `
SELECT COALESCE(SUM(COALESCE(required_cores, ?)), 0)
FROM   tasks
WHERE  assigned_worker_id = ?
  AND  status IN ('assigned', 'running')`

	// sqlLeaseTaskWrite is the ready → assigned write shared by
	// sqlLeaseReadyTask and sqlLeaseTaskGuarded (lease.go), which differ only
	// in the guard they append to it. The target is aliased t so a guard can
	// use sqlLeasableTask's t.-qualified columns.
	sqlLeaseTaskWrite = `
UPDATE tasks AS t
SET    status = 'assigned', assigned_worker_id = ?, assigned_at = ?, updated_at = ?, unschedulable_reason = ''
WHERE  t.id = ?`

	sqlLeaseReadyTask = sqlLeaseTaskWrite + ` AND t.status = 'ready'`

	// sqlCloseAttemptAsFailed closes a running attempt as failed, stamping
	// ended_at, and coalescing exit_code/session_id/message so a nil exit code
	// or empty session/message leaves the existing value intact. The
	// `status = 'running'` guard makes this a no-op (0 rows) on a redelivery
	// whose attempt is already terminal — the signal RecordTaskFailure uses to
	// avoid double-counting.
	sqlCloseAttemptAsFailed = `
UPDATE task_attempts
SET    status = 'failed',
       ended_at = ?,
       exit_code = COALESCE(?, exit_code),
       session_id = COALESCE(NULLIF(?, ''), session_id),
       message = COALESCE(NULLIF(?, ''), message)
WHERE  id = ? AND status = 'running'`

	sqlRecordTaskFailure = `
UPDATE tasks SET failed_attempts = failed_attempts + 1, updated_at = ?
WHERE id = ? RETURNING failed_attempts, job_id`

	sqlIncrementJobFailure = `
UPDATE jobs SET failed_attempts = failed_attempts + 1, updated_at = ?
WHERE id = ? RETURNING failed_attempts`

	// sqlReadFailureCounts reads the current task and job FailedAttempts without
	// incrementing — used on a redelivery where the attempt is already terminal
	// so the caller's retry/park decision stays stable. No rows ⇒ ErrNotFound.
	sqlReadFailureCounts = `
SELECT t.failed_attempts, j.failed_attempts
FROM   tasks t JOIN jobs j ON t.job_id = j.id
WHERE  t.id = ?`

	// sqlRequeueTaskForRetry returns a task to ready, clearing its worker
	// assignment and stamping retry_after so [Store.ListReadyTasks] excludes it
	// until the backoff elapses. Guarded to assigned/running — the only states
	// a genuine in-flight failure can arrive from — so a stale or redelivered
	// failure report can never resurrect a canceled/succeeded task or yank a
	// task that has already been returned to ready. Also guarded on the
	// reporting attempt still being the task's latest (sqlIsLatestAttempt,
	// embedded): a reclaim and a new lease landing
	// between RecordTaskFailure and this statement must not return the new
	// lease to ready (H4a2 §4.3). The attempt need not still be running: a
	// crash-recovery redelivery finds it already closed as failed by the first
	// delivery, yet still the latest, and must requeue.
	sqlRequeueTaskForRetry = `
UPDATE tasks
SET status = 'ready', assigned_worker_id = NULL, assigned_at = NULL,
    retry_after = ?, updated_at = ?, failure_reason = ''
WHERE id = ? AND status IN ('assigned', 'running')
  AND (` + sqlIsLatestAttempt + `)`

	// sqlRetryTasksPrefix revives the selected failed/canceled tasks, clearing
	// the genuine-failure state a manual retry gives a clean slate:
	// failed_attempts back to zero and any backoff stamp removed. A task whose
	// step is still ready (a sibling in flight) is revived ready, since nothing
	// releases a pending task in a ready step (H4a2 §3.4); any other is revived
	// pending for ResolveDependencies to release. 'running' is treated as ready
	// for rows written outside the store operations (no step is running, H4a
	// D4). The optional "AND id IN (?, …)" suffix and sqlRetryTasksReturning are
	// appended at call time.
	sqlRetryTasksPrefix = `
UPDATE tasks
SET    status = CASE WHEN (SELECT s.status FROM steps s WHERE s.id = tasks.step_id) IN ('ready', 'running')
                     THEN 'ready' ELSE 'pending' END,
       updated_at = ?, unschedulable_reason = '',
       failed_attempts = 0, retry_after = NULL, failure_reason = ''
WHERE  job_id = ? AND status IN ('failed', 'canceled')`

	// sqlRetryTasksReturning hands back exactly the revived rows, as they are
	// after the update (I2).
	sqlRetryTasksReturning = ` RETURNING ` + taskCols

	// sqlRetryResetSteps resets any terminal step that now owns a pending task
	// (i.e. a task this retry just revived) back to pending.
	sqlRetryResetSteps = `
UPDATE steps
SET    status = 'pending', updated_at = ?
WHERE  job_id = ? AND status IN ('failed', 'canceled')
  AND  EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id AND t.status = 'pending')`

	// sqlRetryResetJob resets the job to pending when it is currently terminal
	// or auto-parked (paused with a park_reason), also clearing its
	// genuine-failure state so the job restarts with a clean failure count and
	// no stale park reason. A manually paused job (empty park_reason) is left
	// paused — retrying its tasks must not override the operator's pause.
	sqlRetryResetJob = `
UPDATE jobs
SET    status = 'pending', completed_at = NULL, updated_at = ?,
       failed_attempts = 0, park_reason = ''
WHERE  id = ?
  AND  (status IN ('failed', 'canceled') OR (status = 'paused' AND park_reason != ''))`
)

func scanTask(row scanner) (store.Task, error) {
	var t store.Task
	var paramsJSON, status string
	var assignedWorkerID, assignedAt sql.NullString
	var createdAt, updatedAt string
	var reqCores sql.NullInt64
	var retryAfter sql.NullString

	if err := row.Scan(
		&t.ID, &t.JobID, &t.StepID, &t.Name, &paramsJSON, &status,
		&assignedWorkerID, &assignedAt, &createdAt, &updatedAt, &reqCores,
		&t.UnschedulableReason, &t.FailedAttempts, &retryAfter, &t.FailureReason,
	); err != nil {
		return store.Task{}, err
	}

	t.Status = store.TaskStatus(status)
	t.AssignedWorkerID = assignedWorkerID.String
	t.AssignedAt = nullTextToTime(assignedAt)
	t.CreatedAt = mustTime(createdAt)
	t.UpdatedAt = mustTime(updatedAt)
	t.RetryAfter = nullTextToTime(retryAfter)
	if reqCores.Valid {
		v := int(reqCores.Int64)
		t.RequiredCores = &v
	}

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
	var reqCores sql.NullInt64
	if task.RequiredCores != nil {
		reqCores = sql.NullInt64{Int64: int64(*task.RequiredCores), Valid: true}
	}
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertTask.QueryRowContext(ctx,
		task.ID, task.JobID, task.StepID, task.Name, paramsJSON, string(task.Status),
		nullString(task.AssignedWorkerID), nullTimeToText(task.AssignedAt), now, now, reqCores,
		task.UnschedulableReason, task.FailedAttempts, nullTimeToText(task.RetryAfter), task.FailureReason)
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
	if len(opts.Statuses) > 0 {
		// Build a parameterised IN clause: status IN (?, ?, …)
		placeholders := make([]byte, 0, 2*len(opts.Statuses)-1)
		for i, s := range opts.Statuses {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, string(s))
		}
		where += ` AND status IN (` + string(placeholders) + `)`
	} else if opts.Status != "" {
		where += ` AND status = ?`
		args = append(args, string(opts.Status))
	}
	if opts.WorkerID != "" {
		where += ` AND assigned_worker_id = ?`
		args = append(args, opts.WorkerID)
	}

	var total int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`+where, args...).Scan(&total); err != nil {
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
	rows, err := s.rdb.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
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
//
// The write is gated by the task state machine
// ([store.ValidateTaskTransition]): a transition the machine does not permit is
// rejected with [store.ErrInvalidTransition] and leaves the row untouched.
//
// Writing the status a task already holds is a no-op, not an error. Task status
// arrives over JetStream, which is at-least-once, so a redelivered message must
// not fail — the consumer would Nak it and redeliver forever.
//
// The write is a compare-and-set on the status that was read (invariant I1, see
// [casTaskStatusTx]), so it is correct under concurrent writers rather than
// only under SQLite's single write connection.
func (s *Store) UpdateTaskStatus(ctx context.Context, id string, status store.TaskStatus) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin tx for update task status: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is best-effort after commit

	if _, err = casTaskStatusTx(ctx, tx, id, status, time.Now().UTC()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit update task status: %w", err)
	}
	return nil
}

// SetTaskUnschedulableReason implements [store.TaskStore]. A task that is no
// longer ready is a guarded no-op returning (false, nil); only an unknown task
// is an error.
func (s *Store) SetTaskUnschedulableReason(ctx context.Context, id, reason string) (bool, error) {
	res, err := s.stmtSetTaskUnschedulableReason.ExecContext(ctx, reason, timeToText(time.Now().UTC()), id)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}
	if _, err := s.GetTask(ctx, id); err != nil {
		return false, err // ErrNotFound
	}
	return false, nil // no longer ready: a guarded no-op (F15)
}

// SetTaskFailureReason implements [store.TaskStore]. An empty reason clears it.
func (s *Store) SetTaskFailureReason(ctx context.Context, id, reason string) error {
	res, err := s.stmtSetTaskFailureReason.ExecContext(ctx, reason, timeToText(time.Now().UTC()), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// SetTaskFailureReasonIfEmpty implements [store.TaskStore]. A zero-row update
// (task unknown or already carrying a reason) is a legitimate no-op, not an error.
func (s *Store) SetTaskFailureReasonIfEmpty(ctx context.Context, id, reason string) error {
	if _, err := s.stmtSetTaskFailureReasonIfEmpty.ExecContext(ctx, reason, timeToText(time.Now().UTC()), id); err != nil {
		return mapErr(err)
	}
	return nil
}

// AssignTask sets a task's worker, assignment time and status to assigned
// unconditionally and clears its unschedulable reason. It returns
// [store.ErrNotFound] when the task does not exist. The scheduler takes tasks
// through [Store.LeaseTask], which guards the same move.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) AssignTask(ctx context.Context, id, workerID string, assignedAt time.Time) error {
	now := timeToText(time.Now().UTC())
	res, err := s.stmtAssignTask.ExecContext(ctx, workerID, timeToText(assignedAt), now, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// ReclaimWorkerTasks returns every assigned or running task of workerID to
// ready and returns how many it reset. It closes no attempts and releases no
// claims; a worker is taken offline through [Store.OfflineStaleWorker] and
// [Store.OfflineWorker], which do all three in one transaction.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) ReclaimWorkerTasks(ctx context.Context, workerID string) (int, error) {
	now := timeToText(time.Now().UTC())
	res, err := s.stmtReclaimWorkerTasks.ExecContext(ctx, now, workerID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ReclaimStaleAssignedTasks implements [store.TaskStore].
//
// One transaction returns the stale assignments to ready, then closes each
// reclaimed task's running attempts as failed and releases those attempts'
// claims (invariant I3). The returned set is the UPDATE's RETURNING set, so it
// is exactly the tasks this call reset (invariant I2); the scheduler never has
// to look an attempt up after the fact, which is what let the old reaper close
// the new attempt of a task that had been leased again in between.
//
// Statement order (spec 4.1): the tasks are reset first and only then are their
// attempts closed and their claims released, the order CancelJobExecution uses.
// The writer this order guards against is the worker's own report on a stale
// assigned task. On Postgres the UPDATE takes each task's row lock, so a running
// or terminal report already holding the row is waited for, and the UPDATE then
// re-checks status = 'assigned' against the committed row and skips a task the
// report moved on. Closing attempts first would let a report move the task
// (assigned to running, say) after its attempt was already closed as failed. A
// LeaseTask is not a competitor here: it moves a ready row, which this UPDATE's
// predicate does not match, and the task it leaves assigned is not stale.
//
// Anchors: none are taken, deliberately. H4c must take them BEFORE the UPDATE,
// and without locking any task row first: read the candidate task IDs and their
// job IDs UNLOCKED, take the job-row anchors sorted by id, then run the UPDATE
// re-guarded as WHERE id IN (candidates) AND status = 'assigned' AND assigned_at
// < cutoff RETURNING. Locking the task rows first (SELECT ... FOR UPDATE on
// tasks) or anchoring after the UPDATE ... RETURNING both take a task row before
// its job row, and every job-level operation locks the job row before its
// tasks, so the two orders can deadlock. The anchors are also what orders the
// reaper against CompleteTaskAttempt, which writes the attempt row before the
// task row, the reverse of this function: both under the same job-row lock,
// they cannot interleave.
func (s *Store) ReclaimStaleAssignedTasks(ctx context.Context, cutoff time.Time) ([]store.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin tx for reclaim stale assigned tasks: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	// Order (spec 4.1): the tasks, then each one's attempts and claims. Server
	// time stamps updated_at, ended_at and released_at alike.
	nowText := timeToText(time.Now().UTC())
	reclaimed, err := queryTasksTx(ctx, tx, sqlReclaimStaleAssignedTasks, nowText, timeToText(cutoff.UTC()))
	if err != nil {
		return nil, fmt.Errorf("sqlite: reclaim stale assigned tasks: %w", err)
	}
	if len(reclaimed) == 0 {
		return nil, nil // nothing was stale; the deferred rollback ends the empty transaction
	}
	for _, t := range reclaimed {
		if err := closeTaskAttemptsTx(ctx, tx, t.ID, store.AttemptStatusFailed, "", nowText); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit reclaim stale assigned tasks: %w", mapErr(err))
	}
	return reclaimed, nil
}

// ListReadyTasks implements [store.TaskStore].
func (s *Store) ListReadyTasks(ctx context.Context, farmID string, now time.Time, limit int) ([]store.Task, error) {
	rows, err := s.stmtListReadyTasks.QueryContext(ctx, farmID, farmID, timeToText(now), limit)
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
// Returns a map of queue ID → leasable ready-task count for the given farm.
// Queues with zero leasable tasks are omitted.
func (s *Store) CountReadyTasksByQueue(ctx context.Context, farmID string, now time.Time) (map[string]int, error) {
	rows, err := s.stmtCountReadyTasksByQueue.QueryContext(ctx, farmID, farmID, timeToText(now.UTC()))
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

// CancelJobTasks cancels every non-terminal task of the job, clearing the worker
// assignment, and returns the ones that were assigned or running with their
// worker intact. It closes no attempts and releases no claims; a job is
// canceled through [Store.CancelJobExecution], which does all three in one
// transaction.
//
// The SELECT and UPDATE execute inside a single SQLite transaction so no
// concurrent scheduler tick can assign a task between observation and
// cancellation. The SELECT is [sqlSelectActiveJobTasks], the one
// [Store.CancelJobExecution] runs, and [queryTasksTx] closes its cursor before
// the UPDATE runs, which avoids any cursor/write contention on the
// single-connection pool.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) CancelJobTasks(ctx context.Context, jobID string, now time.Time, reason string) ([]store.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin tx for cancel job tasks: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is best-effort after commit

	// Capture tasks that are currently assigned or running so the scheduler can
	// publish cancel signals to their workers. They are read into a slice before
	// the UPDATE, so the cursor is closed by the time we write.
	active, err := queryTasksTx(ctx, tx, sqlSelectActiveJobTasks, jobID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: select active tasks for job %s: %w", jobID, err)
	}

	// Transition all non-terminal tasks to canceled, clearing the worker
	// assignment so stale heartbeat messages cannot re-assign them.
	if _, err = tx.ExecContext(ctx, sqlCancelJobTasks, timeToText(now), reason, jobID); err != nil {
		return nil, fmt.Errorf("sqlite: cancel tasks for job %s: %w", jobID, mapErr(err))
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit cancel job tasks: %w", err)
	}
	return active, nil
}

// RetryTasks implements [store.TaskStore].
func (s *Store) RetryTasks(ctx context.Context, jobID string, taskIDs []string, now time.Time) ([]store.Task, error) {
	nowText := timeToText(now.UTC())

	// Build the optional "AND id IN (?, …)" suffix and its bound args.
	// A non-nil but empty slice means "filter to exactly these (zero) IDs" →
	// nothing to revive. Short-circuit so we never emit "AND id IN ()".
	inSuffix := ""
	idArgs := make([]any, 0, len(taskIDs))
	if taskIDs != nil {
		if len(taskIDs) == 0 {
			return nil, nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(taskIDs)), ",")
		inSuffix = " AND id IN (" + placeholders + ")"
		for _, id := range taskIDs {
			idArgs = append(idArgs, id)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin tx for retry tasks: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is best-effort after commit

	// G5: the anchor is the job row, taken first. The revive UPDATE ... RETURNING
	// is the returned set (I2), so there is no SELECT for a concurrent writer to
	// slip in behind.
	if err := lockAnchors(ctx, tx, jobAnchor(jobID)); err != nil {
		return nil, err
	}

	updArgs := append([]any{nowText, jobID}, idArgs...)
	revived, err := queryTasksTx(ctx, tx, sqlRetryTasksPrefix+inSuffix+sqlRetryTasksReturning, updArgs...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: retry tasks for job %s: %w", jobID, err)
	}
	if len(revived) == 0 {
		if err = tx.Commit(); err != nil {
			return nil, fmt.Errorf("sqlite: commit retry tasks (no-op): %w", err)
		}
		return nil, nil
	}

	if _, err = tx.ExecContext(ctx, sqlRetryResetSteps, nowText, jobID); err != nil {
		return nil, fmt.Errorf("sqlite: reset steps for job %s: %w", jobID, mapErr(err))
	}
	if _, err = tx.ExecContext(ctx, sqlRetryResetJob, nowText, jobID); err != nil {
		return nil, fmt.Errorf("sqlite: reset job %s: %w", jobID, mapErr(err))
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit retry tasks: %w", err)
	}
	return revived, nil
}

// TransitionStepPendingTasks moves every pending task of the step to `to`
// without touching the step or its job. The UPDATE ... RETURNING runs as one
// statement so the transition is atomic and covers every pending task of the
// step regardless of count.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) TransitionStepPendingTasks(ctx context.Context, stepID string, to store.TaskStatus, failureReason string) ([]store.Task, error) {
	rows, err := s.db.QueryContext(ctx, sqlTransitionStepPendingTasks, string(to), timeToText(time.Now().UTC()), failureReason, stepID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: transition pending tasks for step %s: %w", stepID, mapErr(err))
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
	return tasks, mapErr(rows.Err())
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

// CountUnschedulableTasksByJob implements [store.TaskStore].
// Returns the number of ready tasks for the given job that carry a non-empty
// unschedulable reason.
func (s *Store) CountUnschedulableTasksByJob(ctx context.Context, jobID string) (int, error) {
	var n int
	err := s.stmtCountUnschedulableTasksByJob.QueryRowContext(ctx, jobID).Scan(&n)
	if err != nil {
		return 0, mapErr(err)
	}
	return n, nil
}

// FailureReasonSummary implements [store.TaskStore].
func (s *Store) FailureReasonSummary(ctx context.Context, jobID string) (store.FailureSummary, error) {
	rows, err := s.rdb.QueryContext(ctx, sqlFailureReasonSummary, jobID)
	if err != nil {
		return store.FailureSummary{}, mapErr(err)
	}
	defer rows.Close()

	var sum store.FailureSummary
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return store.FailureSummary{}, err
		}
		if sum.DistinctReasons == 0 {
			sum.DominantReason = reason // first row = highest n (query is ordered)
		}
		sum.DistinctReasons++
		sum.FailedCount += n
	}
	return sum, mapErr(rows.Err())
}

// CommittedCores implements [store.TaskStore].
func (s *Store) CommittedCores(ctx context.Context, workerID string, fullMachineCost int) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, sqlCommittedCores, fullMachineCost, workerID).Scan(&n)
	return n, mapErr(err)
}

// LeaseReadyTask moves a ready task to assigned without an attempt, a cap
// check or a claim, and reports whether the task was still ready. The
// scheduler leases through [Store.LeaseTask], which does all of that in one step.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) LeaseReadyTask(ctx context.Context, taskID, workerID string, now time.Time) (bool, error) {
	nowText := timeToText(now.UTC())
	res, err := s.db.ExecContext(ctx, sqlLeaseReadyTask, workerID, nowText, nowText, taskID)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// RecordTaskFailure implements [store.TaskStore].
//
// The attempt close, task increment, and job increment run inside a single
// transaction. The counter increments are GATED on the attempt's running→failed
// transition: closing the attempt (which only affects a row while it is still
// running) is what authorizes the increment, so an at-least-once redelivery of
// the same status message — whose attempt is already terminal — counts zero
// rows, skips both increments, and returns the current counts instead. This
// makes the failure count exactly-once per attempt, and keeps the two counters
// in lockstep: either both increment or neither does.
//
// Invariant I3: the same transaction releases the attempt's claims, so a
// closed attempt never keeps a usage slot. Anchor: the task's job row, taken
// before the first write. The statements run attempt close, claim release, then
// the counters; the claim release does not depend on the counters.
func (s *Store) RecordTaskFailure(
	ctx context.Context,
	attemptID, taskID string,
	exitCode *int,
	sessionID, message string,
	now time.Time,
) (taskFailed, jobFailed int, firstClose bool, err error) {
	nowText := timeToText(now.UTC())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, false, fmt.Errorf("sqlite: begin tx for record task failure: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	var anchorJobID string
	if err = tx.QueryRowContext(ctx, sqlTaskJobID, taskID).Scan(&anchorJobID); err != nil {
		return 0, 0, false, mapErr(err) // no such task => ErrNotFound
	}
	if err := lockAnchors(ctx, tx, jobAnchor(anchorJobID)); err != nil {
		return 0, 0, false, err
	}

	res, err := tx.ExecContext(ctx, sqlCloseAttemptAsFailed, nowText, nullInt(exitCode), sessionID, message, attemptID)
	if err != nil {
		return 0, 0, false, mapErr(err)
	}
	closed, err := res.RowsAffected()
	if err != nil {
		return 0, 0, false, err
	}
	// Invariant I3: a closed attempt holds no claims. Released unconditionally,
	// so a redelivery is also safe; the UPDATE is a no-op when nothing is active.
	// released_at is server time, not the caller's now (a worker-reported time).
	if _, err = tx.ExecContext(ctx, sqlReleaseAttemptClaims, timeToText(time.Now().UTC()), attemptID); err != nil {
		return 0, 0, false, mapErr(err)
	}

	if closed == 0 {
		// Redelivery (or unknown attempt): the attempt is already terminal, so
		// do NOT re-count. Return the current counters so the caller's decision
		// is identical to the first delivery. No matching task ⇒ ErrNotFound.
		if err = tx.QueryRowContext(ctx, sqlReadFailureCounts, taskID).Scan(&taskFailed, &jobFailed); err != nil {
			return 0, 0, false, mapErr(err)
		}
		if err = tx.Commit(); err != nil {
			return 0, 0, false, fmt.Errorf("sqlite: commit record task failure: %w", mapErr(err))
		}
		return taskFailed, jobFailed, false, nil
	}

	// First close of this attempt: count the failure exactly once.
	var jobID string
	if err = tx.QueryRowContext(ctx, sqlRecordTaskFailure, nowText, taskID).Scan(&taskFailed, &jobID); err != nil {
		return 0, 0, false, mapErr(err)
	}
	if err = tx.QueryRowContext(ctx, sqlIncrementJobFailure, nowText, jobID).Scan(&jobFailed); err != nil {
		return 0, 0, false, mapErr(err)
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, false, fmt.Errorf("sqlite: commit record task failure: %w", mapErr(err))
	}
	return taskFailed, jobFailed, true, nil
}

// RequeueTaskForRetry implements [store.TaskStore]. Zero rows (task missing,
// no longer assigned/running, or attemptID not the task's latest attempt) is a
// legitimate no-op reported as false — NOT an error, so a stale redelivery
// never naks into a redelivery loop.
func (s *Store) RequeueTaskForRetry(ctx context.Context, taskID, attemptID string, retryAfter, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, sqlRequeueTaskForRetry,
		timeToText(retryAfter.UTC()), timeToText(now.UTC()), taskID, attemptID, taskID, taskID)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
