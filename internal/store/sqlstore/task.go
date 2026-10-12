// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
	// scheduled. The status test is evaluated inside the UPDATE (I1).
	sqlSetTaskUnschedulableReason = `
UPDATE tasks SET unschedulable_reason = ?, updated_at = ? WHERE id = ? AND status = 'ready'`

	sqlSetTaskFailureReason = `
UPDATE tasks SET failure_reason = ?, updated_at = ? WHERE id = ?`

	// Joins to jobs, queues, and steps to apply the full selection ordering:
	//   1. j.priority DESC        — highest-priority jobs first
	//   2. j.created_at ASC       — earlier-submitted jobs win ties
	//   3. s.step_order ASC       — within a job, earlier steps run first
	//   4. t.created_at ASC       — stable tiebreaker within a step
	//
	// All task columns are fully qualified (t.) to avoid ambiguity because
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
  AND  NOT q.paused
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

	// sqlStaleAssignedCandidates lists the stale assignments and their jobs,
	// unlocked: the reaper locks the jobs and then resets each candidate under
	// sqlReclaimStaleAssignedSuffix's re-check. Only 'assigned' (never
	// 'running'), so an in-progress task is never disturbed by the timer.
	sqlStaleAssignedCandidates = `
SELECT id, job_id FROM tasks
WHERE  status = 'assigned' AND assigned_at IS NOT NULL AND assigned_at < ?
ORDER BY id`

	// sqlReclaimStaleAssignedSuffix follows "WHERE id IN (…)": the candidates are
	// reset only if still stale and assigned, re-checked against the committed
	// row, and RETURNING hands back exactly the rows changed (I2). Binds: cutoff.
	sqlReclaimStaleAssignedSuffix = `
  AND  status = 'assigned' AND assigned_at IS NOT NULL AND assigned_at < ?
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

	// Cancels all non-terminal tasks for a job. [Store.CancelJobExecution] runs
	// it after SELECTing the active tasks within the same transaction to capture
	// worker IDs before this UPDATE clears them.
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

	// sqlLeaseTaskWrite is the ready → assigned write that sqlLeaseTaskGuarded
	// (lease.go) appends its guard to. The target is aliased t so the guard can
	// use sqlLeasableTask's t.-qualified columns.
	sqlLeaseTaskWrite = `
UPDATE tasks AS t
SET    status = 'assigned', assigned_worker_id = ?, assigned_at = ?, updated_at = ?, unschedulable_reason = ''
WHERE  t.id = ?`

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
	// embedded): a reclaim and a new lease landing between RecordTaskFailure
	// and this statement must not return the new lease to ready. The attempt
	// need not still be running: a crash-recovery redelivery finds it already
	// closed as failed by the first delivery, yet still the latest, and must
	// requeue.
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
	// releases a pending task in a ready step; any other is revived pending for
	// ResolveDependencies to release. 'running' is treated as ready for rows
	// written outside the store operations (no store operation writes it). The
	// optional "AND id IN (?, …)" suffix and sqlRetryTasksReturning are
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
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`+where, args...).Scan(&total); err != nil {
		return store.Page[store.Task]{}, mapErr(err)
	}

	col, ok := taskSortColumns[opts.SortBy]
	if !ok {
		col = "created_at"
	}
	dir := sortDirKeyword(opts.SortDir)

	// col comes from taskSortColumns (hard-coded allow-list); dir is "ASC" or
	// "DESC" from sortDirKeyword; orderByClause renders both, with an id
	// tie-break; where uses only ? placeholders for user values.
	q := `SELECT ` + taskCols + ` FROM tasks` + where +
		orderByClause(col, dir, false) +
		` LIMIT ? OFFSET ?`
	rows, err := s.r.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
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

// SetTaskUnschedulableReason implements [store.TaskStore]. A task that is no
// longer ready is a guarded no-op returning (false, nil); only an unknown task
// is an error.
func (s *Store) SetTaskUnschedulableReason(ctx context.Context, id, reason string) (bool, error) {
	res, err := s.stmtSetTaskUnschedulableReason.ExecContext(ctx, reason, TimeText(time.Now().UTC()), id)
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
	return false, nil // no longer ready: a guarded no-op
}

// reclaimBatch bounds how many candidates one transaction resets, keeping
// the IN list and the locks one transaction holds small.
const reclaimBatch = 500

// ReclaimStaleAssignedTasks implements [store.TaskStore].
//
// It lists the stale assignments unlocked, then, a batch at a time, locks the
// batch's job rows sorted by id and resets the batch with a re-guarded UPDATE
// … RETURNING, then closes each reset task's running attempts and releases
// their claims (I3). The returned set is the RETURNING set: exactly what was
// reset (I2), so the scheduler never has to look an attempt up after the fact
// and cannot close the new attempt of a task leased again in between. Each
// batch is its own committed transaction, so on error the result lists the
// tasks earlier batches already reclaimed.
//
// Order: no task row is locked before its job row (every job-level operation
// locks the job first, so the reverse would deadlock), and the job lock is what
// orders the reaper against CompleteTaskAttempt, which writes the attempt
// before the task, the reverse of the reaper. Within the transaction the tasks
// are reset before their attempts are closed, so a worker's report already
// holding a task row is waited for and the re-check then skips the task it
// moved; closing attempts first would let a report move the task (assigned to
// running, say) after its attempt was already closed as failed. A LeaseTask is
// not a competitor: it moves a ready row, which the re-check does not match,
// and the task it leaves assigned is not stale.
func (s *Store) ReclaimStaleAssignedTasks(ctx context.Context, cutoff time.Time) ([]store.Task, error) {
	cutoffText := TimeText(cutoff.UTC())
	cands, err := s.staleAssignedCandidates(ctx, cutoffText)
	if err != nil {
		return nil, err
	}
	var reclaimed []store.Task
	for batch := range slices.Chunk(cands, reclaimBatch) {
		got, err := s.reclaimStaleBatch(ctx, batch, cutoffText)
		if err != nil {
			return reclaimed, err
		}
		reclaimed = append(reclaimed, got...)
	}
	return reclaimed, nil
}

// staleCandidate is a stale assignment the reaper will re-check under its job
// row's lock.
type staleCandidate struct{ taskID, jobID string }

// staleAssignedCandidates lists, in task id order, the stale assignments and
// their jobs, unlocked. The read goes through the write pool so that it sees
// this store's own committed writes. Correctness does not rest on how fresh the
// list is: each candidate is reset only under its job's lock and the UPDATE's
// re-check, so a stale entry is skipped and a missing one is found by the next
// sweep.
func (s *Store) staleAssignedCandidates(ctx context.Context, cutoffText string) ([]staleCandidate, error) {
	rows, err := s.w.QueryContext(ctx, sqlStaleAssignedCandidates, cutoffText)
	if err != nil {
		return nil, fmt.Errorf("store: list stale assigned tasks: %w", mapErr(err))
	}
	defer rows.Close()
	var out []staleCandidate
	for rows.Next() {
		var c staleCandidate
		if err := rows.Scan(&c.taskID, &c.jobID); err != nil {
			return nil, fmt.Errorf("store: scan stale assigned task: %w", err)
		}
		out = append(out, c)
	}
	return out, mapErr(classifyErr(s.d, rows.Err()))
}

// reclaimStaleBatch resets one batch of candidates in one transaction: their
// job rows locked sorted by id, then the re-guarded reset, then each reset
// task's attempts and claims. Server time stamps updated_at, ended_at and
// released_at alike.
func (s *Store) reclaimStaleBatch(ctx context.Context, batch []staleCandidate, cutoffText string) ([]store.Task, error) {
	jobIDs := make([]string, 0, len(batch))
	args := make([]any, 0, len(batch)+2)
	nowText := TimeText(time.Now().UTC())
	args = append(args, nowText) // sqlReclaimTaskSet's updated_at
	for _, c := range batch {
		jobIDs = append(jobIDs, c.jobID)
		args = append(args, c.taskID)
	}
	args = append(args, cutoffText)
	slices.Sort(jobIDs)
	jobIDs = slices.Compact(jobIDs)
	anchors := make([]Anchor, 0, len(jobIDs))
	for _, id := range jobIDs {
		anchors = append(anchors, jobAnchor(id))
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
	// placeholders holds only "?" marks; every value is bound.
	query := sqlReclaimTaskSet + ` WHERE id IN (` + placeholders + `)` + sqlReclaimStaleAssignedSuffix

	var reclaimed []store.Task
	err := s.inTx(ctx, "reclaim stale assigned tasks", func(tx *dbTx) (txOutcome, error) {
		reclaimed = nil
		if err := tx.lockAnchors(ctx, anchors...); err != nil {
			return txRollback, err
		}
		var err error
		reclaimed, err = queryTasksTx(ctx, tx, query, args...)
		if err != nil {
			return txRollback, fmt.Errorf("store: reclaim stale assigned tasks: %w", err)
		}
		if len(reclaimed) == 0 {
			reclaimed = nil
			return txRollback, nil // every candidate moved on; the rollback ends the transaction
		}
		for _, t := range reclaimed {
			if err := closeTaskAttemptsTx(ctx, tx, t.ID, store.AttemptStatusFailed, "", nowText); err != nil {
				return txRollback, err
			}
		}
		return txCommit, nil
	})
	if err != nil {
		return nil, err
	}
	return reclaimed, nil
}

// ListReadyTasks implements [store.TaskStore].
func (s *Store) ListReadyTasks(ctx context.Context, farmID string, now time.Time, limit int) ([]store.Task, error) {
	rows, err := s.stmtListReadyTasks.QueryContext(ctx, farmID, farmID, TimeText(now), limit)
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
	rows, err := s.stmtCountReadyTasksByQueue.QueryContext(ctx, farmID, farmID, TimeText(now.UTC()))
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

// RetryTasks implements [store.TaskStore].
func (s *Store) RetryTasks(ctx context.Context, jobID string, taskIDs []string, now time.Time) ([]store.Task, error) {
	nowText := TimeText(now.UTC())

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

	var revived []store.Task
	err := s.inTx(ctx, "retry tasks", func(tx *dbTx) (txOutcome, error) {
		// The anchor is the job row, taken first. The revive UPDATE ... RETURNING
		// is the returned set (I2), so there is no SELECT for a concurrent writer to
		// slip in behind.
		if err := tx.lockAnchors(ctx, jobAnchor(jobID)); err != nil {
			return txRollback, err
		}

		updArgs := append([]any{nowText, jobID}, idArgs...)
		var err error
		revived, err = queryTasksTx(ctx, tx, sqlRetryTasksPrefix+inSuffix+sqlRetryTasksReturning, updArgs...)
		if err != nil {
			return txRollback, fmt.Errorf("store: retry tasks for job %s: %w", jobID, err)
		}
		if len(revived) == 0 {
			revived = nil
			return txCommit, nil
		}

		if _, err = tx.ExecContext(ctx, sqlRetryResetSteps, nowText, jobID); err != nil {
			return txRollback, fmt.Errorf("store: reset steps for job %s: %w", jobID, mapErr(err))
		}
		if _, err = tx.ExecContext(ctx, sqlRetryResetJob, nowText, jobID); err != nil {
			return txRollback, fmt.Errorf("store: reset job %s: %w", jobID, mapErr(err))
		}
		return txCommit, nil
	})
	if err != nil {
		return nil, err
	}
	return revived, nil
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
	rows, err := s.r.QueryContext(ctx, sqlFailureReasonSummary, jobID)
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
	err := s.r.QueryRowContext(ctx, sqlCommittedCores, fullMachineCost, workerID).Scan(&n)
	return n, mapErr(err)
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
	nowText := TimeText(now.UTC())

	err = s.inTx(ctx, "record task failure", func(tx *dbTx) (txOutcome, error) {
		taskFailed, jobFailed, firstClose = 0, 0, false
		if err := lockTaskJobTx(ctx, tx, taskID); err != nil {
			return txRollback, err // no such task => ErrNotFound
		}

		res, err := tx.ExecContext(ctx, sqlCloseAttemptAsFailed, nowText, nullInt(exitCode), sessionID, message, attemptID)
		if err != nil {
			return txRollback, mapErr(err)
		}
		closed, err := res.RowsAffected()
		if err != nil {
			return txRollback, err
		}
		// Invariant I3: a closed attempt holds no claims. Released unconditionally,
		// so a redelivery is also safe; the UPDATE is a no-op when nothing is active.
		// released_at is server time, not the caller's now (a worker-reported time).
		if _, err = tx.ExecContext(ctx, sqlReleaseAttemptClaims, TimeText(time.Now().UTC()), attemptID); err != nil {
			return txRollback, mapErr(err)
		}

		if closed == 0 {
			// Redelivery (or unknown attempt): the attempt is already terminal, so
			// do NOT re-count. Return the current counters so the caller's decision
			// is identical to the first delivery. No matching task ⇒ ErrNotFound.
			if err = tx.QueryRowContext(ctx, sqlReadFailureCounts, taskID).Scan(&taskFailed, &jobFailed); err != nil {
				return txRollback, mapErr(err)
			}
			return txCommit, nil
		}

		// First close of this attempt: count the failure exactly once.
		var jobID string
		if err = tx.QueryRowContext(ctx, sqlRecordTaskFailure, nowText, taskID).Scan(&taskFailed, &jobID); err != nil {
			return txRollback, mapErr(err)
		}
		if err = tx.QueryRowContext(ctx, sqlIncrementJobFailure, nowText, jobID).Scan(&jobFailed); err != nil {
			return txRollback, mapErr(err)
		}
		firstClose = true
		return txCommit, nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	return taskFailed, jobFailed, firstClose, nil
}

// RequeueTaskForRetry implements [store.TaskStore]. Zero rows (task missing,
// no longer assigned/running, or attemptID not the task's latest attempt) is a
// legitimate no-op reported as false: NOT an error, so a stale redelivery
// never naks into a redelivery loop.
//
// Anchor: the task's job row, so the latest-attempt check in the guard cannot
// be overtaken by a lease committing a new attempt (a lease holds the job row
// shared).
func (s *Store) RequeueTaskForRetry(ctx context.Context, taskID, attemptID string, retryAfter, now time.Time) (bool, error) {
	var requeued bool
	err := s.inTx(ctx, "requeue task for retry", func(tx *dbTx) (txOutcome, error) {
		requeued = false
		if err := lockTaskJobTx(ctx, tx, taskID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return txRollback, nil // missing task: a no-op, not an error
			}
			return txRollback, err
		}
		res, err := tx.ExecContext(ctx, sqlRequeueTaskForRetry,
			TimeText(retryAfter.UTC()), TimeText(now.UTC()), taskID, attemptID, taskID, taskID)
		if err != nil {
			return txRollback, mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return txRollback, err
		}
		requeued = n > 0
		return txCommit, nil
	})
	return requeued, err
}
