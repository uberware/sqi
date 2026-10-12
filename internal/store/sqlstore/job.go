// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// jobCols is every column scanJob reads, in scan order. declared_extensions is
// on it deliberately: the scheduler's lease path reads a job's declared OpenJD
// extensions once per candidate task, and it must ride along on the SELECT that
// already fetches the row rather than becoming a second query (see
// internal/scheduler/exprcaps.go).
const jobCols = `
	id, farm_id, queue_id, name, owner, submitter, priority, status, project,
	raw_template, template_format, parameters, created_at, updated_at, started_at, completed_at,
	failed_attempts, max_attempts, retry_delay_seconds, failure_limit, park_reason,
	declared_extensions`

const (
	sqlInsertJob = `
INSERT INTO jobs (
	id, farm_id, queue_id, name, owner, submitter, priority, status, project,
	raw_template, template_format, parameters, created_at, updated_at, started_at, completed_at,
	failed_attempts, max_attempts, retry_delay_seconds, failure_limit, park_reason,
	declared_extensions)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + jobCols

	sqlGetJob = `SELECT ` + jobCols + ` FROM jobs WHERE id = ?`

	// sqlUpdateJob updates only the mutable user-settable fields of a job.
	// status, started_at, and completed_at are lifecycle columns managed
	// exclusively by the status operations (FinalizeJob, CancelJobStatus, ...)
	// and are intentionally excluded here — writing them via UpdateJob would
	// race with concurrent scheduler status transitions. failed_attempts and
	// park_reason are likewise lifecycle-managed (by the scheduler's
	// failure-limit sweep) and are excluded for the same reason; max_attempts,
	// retry_delay_seconds, and failure_limit are the user-settable retry-policy
	// overrides.
	//
	// declared_extensions is likewise excluded: it is derived from the template
	// at submission and is not user-settable. raw_template is on the SET list,
	// but the sole production caller (internal/api's PATCH handler)
	// round-trips the value it just read, so the pair cannot drift. A caller
	// that rewrites raw_template here would have to write the extension list
	// with it.
	sqlUpdateJob = `
UPDATE jobs
SET farm_id = ?, queue_id = ?, name = ?, owner = ?, submitter = ?, priority = ?,
	project = ?, raw_template = ?, template_format = ?,
	max_attempts = ?, retry_delay_seconds = ?, failure_limit = ?,
	updated_at = ?
WHERE id = ?
RETURNING ` + jobCols

	// sqlCancelJobStatus transitions a job to 'canceled' only when it is not
	// already in a terminal state.  Zero rows affected means the job was either
	// not found or already terminal; the caller distinguishes these two cases by
	// following up with a GetJob when rows == 0.
	sqlCancelJobStatus = `
UPDATE jobs
SET status       = 'canceled',
	completed_at = ?,
	updated_at   = ?
WHERE id = ?
  AND status NOT IN ('completed', 'failed', 'canceled')`

	// sqlStalledJobPredicate is a running job with no task assigned or running
	// and at least one waiting (ready or pending): stalled, so the sweep shows
	// it pending again. A job whose tasks are all terminal is left for the
	// completion logic to finalize. It names jobs.id, so it is only valid where
	// jobs is in scope.
	sqlStalledJobPredicate = `jobs.status = 'running'
  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = jobs.id AND t.status IN ('assigned', 'running'))
  AND EXISTS     (SELECT 1 FROM tasks t WHERE t.job_id = jobs.id AND t.status IN ('ready', 'pending'))`

	// sqlStalledJobCandidates lists the stalled jobs, unlocked: each is then
	// locked and re-checked before it is demoted.
	sqlStalledJobCandidates = `SELECT id FROM jobs WHERE ` + sqlStalledJobPredicate + ` ORDER BY id`

	// sqlDemoteStalledJob demotes one job if it is still stalled. It runs under
	// the job's lock, and its predicate is evaluated afresh then, so a lease that
	// committed in between (it holds the job row shared) is seen. started_at is
	// intentionally preserved. RETURNING yields the job ID only when a row was
	// demoted, so the caller can fan out the WS event.
	sqlDemoteStalledJob = `
UPDATE jobs SET status = 'pending', updated_at = ?
WHERE  id = ? AND ` + sqlStalledJobPredicate + `
RETURNING id`

	// Job-deletion cascade. Each statement scopes to one job via job_id (or a
	// task subquery for grandchild tables). Run inside a single transaction in
	// this exact order so enforced foreign keys never block a delete.
	// sqlLockJobLoggedTasks selects each of a job's tasks that has an attempt:
	// every task a log line can reference. With the dialect's ForUpdate clause
	// appended it locks them (see lockJobLoggedTasksTx).
	sqlLockJobLoggedTasks = `
SELECT 1 FROM tasks t
WHERE t.job_id = ? AND EXISTS (SELECT 1 FROM task_attempts ta WHERE ta.task_id = t.id)`

	sqlDeleteJobCheckouts = `
DELETE FROM usage_claims
WHERE task_attempt_id IN (
	SELECT ta.id FROM task_attempts ta
	JOIN tasks t ON t.id = ta.task_id
	WHERE t.job_id = ?)`

	sqlDeleteJobTaskLogs = `
DELETE FROM task_logs
WHERE task_id IN (SELECT id FROM tasks WHERE job_id = ?)`

	sqlDeleteJobAttempts = `
DELETE FROM task_attempts
WHERE task_id IN (SELECT id FROM tasks WHERE job_id = ?)`

	sqlDeleteJobTasks        = `DELETE FROM tasks WHERE job_id = ?`
	sqlDeleteJobSteps        = `DELETE FROM steps WHERE job_id = ?`
	sqlDeleteJobDependencies = `DELETE FROM job_dependencies WHERE job_id = ?`
	sqlDeleteJobRow          = `DELETE FROM jobs WHERE id = ?`

	sqlInsertJobDependency = `
INSERT INTO job_dependencies (job_id, depends_on_job_id, created_at)
VALUES (?, ?, ?)
ON CONFLICT DO NOTHING`

	sqlListJobDependencyIDs = `
SELECT depends_on_job_id FROM job_dependencies
WHERE job_id = ? ORDER BY depends_on_job_id`

	sqlListDependents = `
SELECT job_id FROM job_dependencies
WHERE depends_on_job_id = ? ORDER BY job_id`

	sqlListBlockedJobs = `SELECT ` + jobCols + ` FROM jobs WHERE status = 'blocked' ORDER BY created_at`

	// Selects terminal jobs whose effective completion time is before the
	// cutoff. The status set is closed by the caller appending 'failed'. The
	// {STATUS} and time predicate are assembled in Go with bound parameters.
	//
	// The trailing NOT EXISTS excludes a candidate that is still referenced as
	// depends_on_job_id by a non-terminal dependent: purging it would leave
	// the dependent's reconciler reading a GetJob ErrNotFound for an upstream
	// that actually succeeded, which the reconciler treats as "missing" and
	// wrongly cancels the dependent. Manual DELETE (DeleteJob) intentionally
	// keeps the cancel-dependents behavior — this guard only applies to the
	// automatic retention sweep.
	sqlSelectExpiredJobsPrefix = `
SELECT id, name, farm_id, queue_id FROM jobs
WHERE status IN (`
	sqlSelectExpiredJobsSuffix = `)
  AND COALESCE(completed_at, updated_at) < ?
  AND NOT EXISTS (
        SELECT 1 FROM job_dependencies jd
        JOIN jobs d ON d.id = jd.job_id
        WHERE jd.depends_on_job_id = jobs.id
          AND d.status NOT IN ('completed', 'failed', 'canceled'))`

	// sqlExpiredJobByID narrows the retention eligibility SELECT to one job,
	// for the per-job re-check immediately before that job's cascade.
	sqlExpiredJobByID = ` AND id = ?`

	// sqlParkJob pauses a job and records why, but only while it is
	// non-terminal — a job that has already reached completed/failed/canceled
	// is left alone (zero rows affected is a legitimate no-op, not an error).
	sqlParkJob = `
UPDATE jobs SET status = 'paused', park_reason = ?, updated_at = ?
WHERE id = ? AND status NOT IN ('completed','failed','canceled')`

	// sqlResumeJob is the inverse of sqlParkJob: only a paused job is resumed.
	// An auto-parked job (non-empty park_reason) also gets its failure counter
	// reset — otherwise the very next genuine failure would re-park it — while
	// a manual pause/resume keeps the accumulated count.
	sqlResumeJob = `
UPDATE jobs
SET status = 'pending', updated_at = ?,
    failed_attempts = CASE WHEN park_reason != '' THEN 0 ELSE failed_attempts END,
    park_reason = ''
WHERE id = ? AND status = 'paused'`
)

// encodeDeclaredExtensions renders a job's declared-extension list for the
// declared_extensions column, preserving the distinction the column exists for:
// "" means NOT RECORDED (the schema default, and what every row written before
// migration 00027 holds), while "[]" means recorded and declaring nothing. A
// recorded nil slice is normalized to "[]" so the not-recorded sentinel has
// exactly one spelling in the database.
func encodeDeclaredExtensions(job store.Job) (string, error) {
	if !job.ExtensionsRecorded {
		return "", nil
	}
	if job.DeclaredExtensions == nil {
		return "[]", nil
	}
	return marshalJSON(job.DeclaredExtensions)
}

// decodeDeclaredExtensions is the inverse of [encodeDeclaredExtensions]. A
// recorded row always yields a non-nil slice, so the nil-ness of the field
// never carries meaning on its own.
func decodeDeclaredExtensions(raw string, job *store.Job) error {
	if raw == "" {
		job.ExtensionsRecorded, job.DeclaredExtensions = false, nil
		return nil
	}
	exts, err := unmarshalJSON(raw, []string{})
	if err != nil {
		return err
	}
	if exts == nil {
		exts = []string{}
	}
	job.ExtensionsRecorded, job.DeclaredExtensions = true, exts
	return nil
}

func scanJob(row scanner) (store.Job, error) {
	var j store.Job
	var status, templateFormat, paramsJSON string
	var createdAt, updatedAt string
	var startedAt, completedAt sql.NullString
	var maxAtt, delay, failLim sql.NullInt64
	var declaredExts string

	if err := row.Scan(
		&j.ID, &j.FarmID, &j.QueueID, &j.Name, &j.Owner, &j.Submitter,
		&j.Priority, &status, &j.Project,
		&j.RawTemplate, &templateFormat, &paramsJSON,
		&createdAt, &updatedAt, &startedAt, &completedAt,
		&j.FailedAttempts, &maxAtt, &delay, &failLim, &j.ParkReason,
		&declaredExts,
	); err != nil {
		return store.Job{}, err
	}

	if err := decodeDeclaredExtensions(declaredExts, &j); err != nil {
		return store.Job{}, err
	}

	j.Status = store.JobStatus(status)
	j.TemplateFormat = store.TemplateFormat(templateFormat)
	j.CreatedAt = mustTime(createdAt)
	j.UpdatedAt = mustTime(updatedAt)
	j.StartedAt = nullTextToTime(startedAt)
	j.CompletedAt = nullTextToTime(completedAt)
	j.MaxAttempts, j.RetryDelaySeconds, j.FailureLimit = intPtr(maxAtt), intPtr(delay), intPtr(failLim)

	params, err := unmarshalJSON(paramsJSON, map[string]string{})
	if err != nil {
		return store.Job{}, err
	}
	j.Parameters = params

	return j, nil
}

// CreateJobSubmission implements [store.JobStore]. Every row one submission
// creates — the job, its dependency edges, its steps and its tasks — is
// written in a single transaction, so a failure at any point leaves nothing
// behind.
//
// Rows are inserted in foreign-key order (job, edges, steps, tasks) and each
// insert runs raw SQL in tx rather than a prepared statement bound to the
// transaction with tx.Stmt, as the other transactional writers here do (see
// casWriteTaskStatus in attemptclose.go).
//
// Each step and task row is stamped with its own, strictly later time from one
// [rowClock] — see insertTasksTx for why sharing one timestamp across the batch
// would be a behavior change, not an optimization.
func (s *Store) CreateJobSubmission(ctx context.Context, sub store.JobSubmission) (store.JobSubmission, error) {
	var out store.JobSubmission
	err := s.inTx(ctx, "create job submission", func(tx *dbTx) (txOutcome, error) {
		now := TimeText(time.Now().UTC())

		// DependsOn is cloned rather than aliased so a caller mutating the returned
		// slice cannot reach back into its own input, matching the fake.
		out = store.JobSubmission{DependsOn: slices.Clone(sub.DependsOn)}
		var err error
		if out.Job, err = insertJobTx(ctx, tx, sub.Job, now); err != nil {
			return txRollback, err
		}
		for _, up := range sub.DependsOn {
			if _, err := tx.ExecContext(ctx, sqlInsertJobDependency, sub.Job.ID, up, now); err != nil {
				return txRollback, fmt.Errorf("store: create job dependency %s->%s: %w", sub.Job.ID, up, mapErr(err))
			}
		}
		// A first, unlocked read of the upstreams fails a doomed submission
		// before it inserts a step or task, where the fake checks too, so a
		// submission that is both unsatisfiable and conflicting is refused with
		// the same error on every backend. It takes no lock: the locked check
		// below is the one that holds.
		if err := checkUpstreamsTx(ctx, tx, sub.DependsOn, false); err != nil {
			return txRollback, err
		}
		var clock rowClock
		if out.Steps, err = insertStepsTx(ctx, tx, &clock, sub.Steps); err != nil {
			return txRollback, err
		}
		if out.Tasks, err = insertTasksTx(ctx, tx, &clock, sub.Tasks, s.taskInsertBatch); err != nil {
			return txRollback, err
		}
		// The upstreams are checked again last, locked, after every insert and
		// just before the commit. job_dependencies has no foreign key on depends_on_job_id,
		// so the edges to a missing or doomed upstream are harmless until then,
		// and an unsatisfiable one rolls the whole submission back. Checking last
		// keeps the upstream rows' share locks (PostgreSQL) to the window between
		// this check and the commit: a large submission still inserting its
		// tasks makes no writer on an upstream job wait.
		if err := checkUpstreamsTx(ctx, tx, sub.DependsOn, true); err != nil {
			return txRollback, err
		}
		return txCommit, nil
	})
	if err != nil {
		return store.JobSubmission{}, err
	}
	return out, nil
}

// checkUpstreamsTx re-checks, inside the submission's transaction, that every
// upstream still exists and has not failed or been canceled. It returns a
// [*store.DependencyUnsatisfiableError] (which matches
// [store.ErrDependencyUnsatisfiable]) naming the first upstream (in ID order)
// that does not qualify and why. The other direction, where an upstream completes
// after the submitter's read, is left to sweepBlockedJobs, which releases the
// job within one sweep tick. A PostgreSQL store locks each upstream row FOR
// SHARE here, in ID order so two submissions naming the same upstreams cannot
// deadlock, when lock is set. CreateJobSubmission calls it locked last, so
// those locks are held only from here to the commit, and unlocked before its
// first step insert, to fail a doomed submission early.
func checkUpstreamsTx(ctx context.Context, tx *dbTx, upstreams []string, lock bool) error {
	ids := slices.Clone(upstreams)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if lock {
		anchors := make([]Anchor, 0, len(ids))
		for _, up := range ids {
			anchors = append(anchors, sharedAnchor(jobAnchor(up)))
		}
		if err := tx.lockAnchors(ctx, anchors...); err != nil {
			return err
		}
	}
	for _, up := range ids {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, up).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return &store.DependencyUnsatisfiableError{UpstreamID: up}
		}
		if err != nil {
			return fmt.Errorf("store: check upstream %s: %w", up, mapErr(err))
		}
		if status == string(store.JobStatusFailed) || status == string(store.JobStatusCanceled) {
			return &store.DependencyUnsatisfiableError{UpstreamID: up, Status: store.JobStatus(status)}
		}
	}
	return nil
}

// insertJobTx inserts one job row inside tx.
func insertJobTx(ctx context.Context, tx *dbTx, job store.Job, now string) (store.Job, error) {
	paramsJSON, err := marshalJSON(job.Parameters)
	if err != nil {
		return store.Job{}, err
	}
	declaredExts, err := encodeDeclaredExtensions(job)
	if err != nil {
		return store.Job{}, err
	}
	row := tx.QueryRowContext(ctx, sqlInsertJob,
		job.ID, job.FarmID, job.QueueID, job.Name, job.Owner, job.Submitter,
		job.Priority, string(job.Status), job.Project,
		job.RawTemplate, string(job.TemplateFormat), paramsJSON,
		now, now,
		nullTimeToText(job.StartedAt), nullTimeToText(job.CompletedAt),
		job.FailedAttempts, nullInt(job.MaxAttempts), nullInt(job.RetryDelaySeconds), nullInt(job.FailureLimit),
		job.ParkReason, declaredExts)
	out, err := scanJob(row)
	return out, mapErr(err)
}

// rowClock stamps the rows of one submission with strictly increasing times.
// Consecutive time.Now calls can return the same instant (Windows advances the
// wall clock in coarse steps), so when the clock has not moved past the
// previous stamp, next returns that stamp plus a nanosecond. [timeLayout] keeps
// all nine digits, so each stamp's text also sorts after the one before.
type rowClock struct{ last time.Time }

func (c *rowClock) next() string {
	now := time.Now().UTC()
	if !now.After(c.last) {
		now = c.last.Add(time.Nanosecond)
	}
	c.last = now
	return TimeText(now)
}

// insertStepsTx inserts every step inside tx, stamping each row with its own
// time from clock (see insertTasksTx).
func insertStepsTx(ctx context.Context, tx *dbTx, clock *rowClock, steps []store.Step) ([]store.Step, error) {
	out := make([]store.Step, 0, len(steps))
	for _, step := range steps {
		dependsOnJSON, err := marshalJSON(step.DependsOn)
		if err != nil {
			return nil, err
		}
		hostReqJSON, err := marshalJSON(step.HostRequirements)
		if err != nil {
			return nil, err
		}
		now := clock.next()
		row := tx.QueryRowContext(ctx, sqlInsertStep,
			step.ID, step.JobID, step.Name, dependsOnJSON,
			step.StepOrder, string(step.Status),
			hostReqJSON, step.ComputeLocation,
			now, now)
		stored, err := scanStep(row)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, stored)
	}
	return out, nil
}

// insertTasksTx inserts every task inside tx, stamping each row with its own
// time from clock, later than every row stamped before it.
//
// The per-row stamp is deliberate. Two consumers depend on tasks within one
// step having DISTINCT created_at values:
//
//   - sqlListReadyTasks (task.go) ends its ORDER BY with "t.created_at ASC",
//     documented there as the stable tiebreaker within a step. One shared
//     timestamp makes that clause inert and frames dispatch in query-planner
//     order rather than expansion order.
//   - ListTasks (task.go) orders by a single column with LIMIT/OFFSET and no
//     secondary key. SQLite's ORDER BY with LIMIT is a partial sort, so equal
//     keys make page boundaries unstable: the same task can appear on two
//     pages while another is omitted.
//
// A UUID tiebreaker in the SQL would be deterministic but would order frames
// randomly rather than in expansion order, which is a different behavior
// change wearing the same clothes. Stamping per row is what preserves the
// behavior submission has always had.
//
// With batchRows above 1 ([WithTaskInsertBatch]) the tasks go in that many to a
// statement; otherwise one statement each, as it always has.
func insertTasksTx(
	ctx context.Context, tx *dbTx, clock *rowClock, tasks []store.Task, batchRows int,
) ([]store.Task, error) {
	if batchRows > 1 {
		return insertTasksBatchedTx(ctx, tx, clock, tasks, batchRows)
	}
	out := make([]store.Task, 0, len(tasks))
	for _, task := range tasks {
		args, err := taskInsertArgs(task, clock.next())
		if err != nil {
			return nil, err
		}
		stored, err := scanTask(tx.QueryRowContext(ctx, sqlInsertTask, args...))
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, stored)
	}
	return out, nil
}

// taskInsertArgs returns the values of one task's insert, in the column order
// of [sqlInsertTasksHead], stamped now.
func taskInsertArgs(task store.Task, now string) ([]any, error) {
	paramsJSON, err := marshalJSON(task.Parameters)
	if err != nil {
		return nil, err
	}
	var reqCores sql.NullInt64
	if task.RequiredCores != nil {
		reqCores = sql.NullInt64{Int64: int64(*task.RequiredCores), Valid: true}
	}
	return []any{
		task.ID, task.JobID, task.StepID, task.Name, paramsJSON, string(task.Status),
		nullString(task.AssignedWorkerID), nullTimeToText(task.AssignedAt), now, now, reqCores,
		task.UnschedulableReason, task.FailedAttempts, nullTimeToText(task.RetryAfter), task.FailureReason,
	}, nil
}

// insertTasksBatchedTx is insertTasksTx at batchRows tasks per statement: the
// same columns, the same per-row stamp from clock (in input order, so the
// ordering insertTasksTx documents holds), and the same rows back, for a
// database where a round trip per task is what a large submission costs.
//
// The statements come in few shapes ([taskBatchRows]): full batches, then the
// remainder in power-of-two pieces.
func insertTasksBatchedTx(
	ctx context.Context, tx *dbTx, clock *rowClock, tasks []store.Task, batchRows int,
) ([]store.Task, error) {
	out := make([]store.Task, 0, len(tasks))
	for len(tasks) > 0 {
		n := taskBatchRows(len(tasks), batchRows)
		chunk := tasks[:n]
		tasks = tasks[n:]
		args := make([]any, 0, len(chunk)*taskInsertColumns)
		for _, task := range chunk {
			row, err := taskInsertArgs(task, clock.next())
			if err != nil {
				return nil, err
			}
			args = append(args, row...)
		}
		stored, err := runInsertTasks(ctx, tx, chunk, args)
		if err != nil {
			return nil, err
		}
		out = append(out, stored...)
	}
	return out, nil
}

// taskBatchRows returns how many of the left tasks the next statement inserts:
// a full batch while one is left, then the largest power of two that fits.
//
// Every distinct row count is a distinct SQL text, and pgx prepares each text
// it has not seen as a named statement on its connection (up to 512 of them,
// each binding up to batchRows*taskInsertColumns parameters). Cutting the
// remainder into plain chunks would let a long-lived connection collect one
// statement per remainder size, up to batchRows-1 of them; powers of two hold
// the batched path to at most 1+log2(batchRows) shapes (10 at 500 rows) for at
// most that many statements per submission. Inserting the remainder a row at a
// time would hold it to two shapes, but at up to batchRows-1 round trips.
func taskBatchRows(left, batchRows int) int {
	if left >= batchRows {
		return batchRows
	}
	return 1 << (bits.Len(uint(left)) - 1)
}

// runInsertTasks inserts chunk's tasks with one statement, whose values are
// args, and returns them as the database stored them, in chunk's order.
func runInsertTasks(ctx context.Context, tx *dbTx, chunk []store.Task, args []any) ([]store.Task, error) {
	rs, err := tx.QueryContext(ctx, sqlInsertTasksHead+taskRowsValues(len(chunk))+sqlInsertTasksTail, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rs.Close()

	stored := make([]store.Task, 0, len(chunk))
	for rs.Next() {
		t, err := scanTask(rs)
		if err != nil {
			return nil, mapErr(classifyErr(tx.d, err))
		}
		stored = append(stored, t)
	}
	if err := rs.Err(); err != nil {
		return nil, mapErr(classifyErr(tx.d, err))
	}
	if len(stored) != len(chunk) {
		return nil, fmt.Errorf("store: insert tasks: %d rows returned for %d inserted", len(stored), len(chunk))
	}
	return inInsertOrder(stored, chunk)
}

// inInsertOrder returns stored in the order of the tasks that were inserted. A
// VALUES list's RETURNING rows come back in list order in practice, but SQL
// does not promise it and a submission's tasks must come back in the order they
// were given, so rows out of place are put back by task ID.
func inInsertOrder(stored, inserted []store.Task) ([]store.Task, error) {
	inOrder := true
	for i := range stored {
		if stored[i].ID != inserted[i].ID {
			inOrder = false
			break
		}
	}
	if inOrder {
		return stored, nil
	}
	byID := make(map[string]store.Task, len(stored))
	for _, t := range stored {
		byID[t.ID] = t
	}
	out := make([]store.Task, len(inserted))
	for i, t := range inserted {
		row, ok := byID[t.ID]
		if !ok {
			return nil, fmt.Errorf("store: insert tasks: no row returned for task %q", t.ID)
		}
		out[i] = row
	}
	return out, nil
}

// taskRowsValues returns the VALUES list of rows tasks: one
// [sqlTaskRowPlaceholders] row each, comma-separated. It is built from the
// constant row alone, so it holds only "?" marks, parentheses, commas and
// spaces; every task value is bound.
func taskRowsValues(rows int) string {
	return strings.TrimSuffix(strings.Repeat(sqlTaskRowPlaceholders+",", rows), ",")
}

// GetJob implements [store.JobStore].
func (s *Store) GetJob(ctx context.Context, id string) (store.Job, error) {
	row := s.stmtGetJob.QueryRowContext(ctx, id)
	out, err := scanJob(row)
	if err != nil {
		return store.Job{}, mapErr(err)
	}

	deps, err := s.ListJobDependencyIDs(ctx, id)
	if err != nil {
		return store.Job{}, err
	}
	out.DependsOn = deps

	return out, nil
}

// ListJobDependencyIDs implements [store.JobStore]. It returns the upstream
// job IDs jobID waits on, ordered by upstream job ID.
func (s *Store) ListJobDependencyIDs(ctx context.Context, jobID string) ([]string, error) {
	return s.queryIDs(ctx, sqlListJobDependencyIDs, jobID)
}

// ListDependents implements [store.JobStore]. It returns the IDs of jobs
// waiting on upstreamJobID, ordered by dependent job ID.
func (s *Store) ListDependents(ctx context.Context, upstreamJobID string) ([]string, error) {
	return s.queryIDs(ctx, sqlListDependents, upstreamJobID)
}

// ListBlockedJobs implements [store.JobStore]. It returns every job currently
// in [store.JobStatusBlocked].
func (s *Store) ListBlockedJobs(ctx context.Context) ([]store.Job, error) {
	rows, err := s.r.QueryContext(ctx, sqlListBlockedJobs)
	if err != nil {
		return nil, fmt.Errorf("store: list blocked jobs: %w", mapErr(err))
	}
	defer rows.Close()

	var jobs []store.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// queryIDs runs a single-column string query and returns the values.
func (s *Store) queryIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query ids: %w", mapErr(err))
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// jobSortColumns maps [store.JobSortField] values to their safe, hard-coded
// SQL column names. Only columns present in this map are accepted; anything
// else falls back to the default. This eliminates any risk of SQL injection
// through the sort field.
var jobSortColumns = map[store.JobSortField]string{
	store.JobSortByCreatedAt: "created_at",
	store.JobSortByPriority:  "priority",
	store.JobSortByStatus:    "status",
	store.JobSortByUpdatedAt: "updated_at",
	store.JobSortByName:      "name",
}

// ListJobs implements [store.JobStore].
// Dynamic filters use parameterised ad-hoc queries due to the variable WHERE
// clause. The sort column is looked up from a hard-coded allow-list before
// being interpolated into the SQL to prevent injection.
func (s *Store) ListJobs(ctx context.Context, opts store.ListJobsOptions) (store.Page[store.Job], error) {
	opts.Pagination.Validate() //nolint:errcheck // Validate only clamps; never errors

	where := ` WHERE 1=1`
	args := make([]any, 0, 5)

	if opts.FarmID != "" {
		where += ` AND farm_id = ?`
		args = append(args, opts.FarmID)
	}
	if opts.QueueID != "" {
		where += ` AND queue_id = ?`
		args = append(args, opts.QueueID)
	}
	if opts.Status != "" {
		where += ` AND status = ?`
		args = append(args, string(opts.Status))
	}
	if opts.Owner != "" {
		where += " AND " + s.d.CIEqual("owner")
		args = append(args, opts.Owner)
	}
	if opts.Project != "" {
		where += ` AND project = ?`
		args = append(args, opts.Project)
	}
	if frag, sargs := searchClause(s.d, []string{"name", "id", "owner", "project"}, opts.Search); frag != "" {
		where += frag
		args = append(args, sargs...)
	}

	// COUNT total matching rows (same WHERE, no ORDER/LIMIT).
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`+where, args...).Scan(&total); err != nil {
		return store.Page[store.Job]{}, mapErr(err)
	}

	// Resolve sort column — default to created_at if unrecognized.
	col, ok := jobSortColumns[opts.SortBy]
	if !ok {
		col = "created_at"
	}
	dir := sortDirKeyword(opts.SortDir)

	// col comes from jobSortColumns (hard-coded allow-list); dir is "ASC" or
	// "DESC" from sortDirKeyword; orderByClause renders both, with an id
	// tie-break; where uses only ? placeholders for user values.
	q := `SELECT ` + jobCols + ` FROM jobs` + where +
		orderByClause(col, dir, false) +
		` LIMIT ? OFFSET ?`
	rows, err := s.r.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
	if err != nil {
		return store.Page[store.Job]{}, mapErr(err)
	}
	defer rows.Close()

	jobs := make([]store.Job, 0)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return store.Page[store.Job]{}, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return store.Page[store.Job]{}, err
	}
	return store.Page[store.Job]{
		Items:  jobs,
		Total:  total,
		Limit:  opts.Pagination.Limit,
		Offset: opts.Pagination.Offset,
	}, nil
}

// UpdateJob implements [store.JobStore].
// Only mutable user-settable fields are written (farm_id, queue_id, name,
// owner, submitter, priority, project, raw_template, template_format,
// max_attempts, retry_delay_seconds, failure_limit).
// status, started_at, completed_at, failed_attempts, and park_reason are
// never touched here; use CancelJobStatus, FinalizeJob, or the
// scheduler's failure-limit sweep for those.
func (s *Store) UpdateJob(ctx context.Context, job store.Job) (store.Job, error) {
	now := TimeText(time.Now().UTC())
	row := s.stmtUpdateJob.QueryRowContext(ctx,
		job.FarmID, job.QueueID, job.Name, job.Owner, job.Submitter, job.Priority,
		job.Project, job.RawTemplate, string(job.TemplateFormat),
		nullInt(job.MaxAttempts), nullInt(job.RetryDelaySeconds), nullInt(job.FailureLimit),
		now, job.ID)
	out, err := scanJob(row)
	return out, mapErr(err)
}

// CancelJobStatus implements [store.JobStore].
//
// It issues a conditional UPDATE that only fires when the job is not already
// in a terminal state. When zero rows are affected the job is either missing
// or already terminal; a follow-up GetJob disambiguates:
//   - not found → [store.ErrNotFound]
//   - already canceled → nil (idempotent)
//   - completed or failed → [store.ErrConflict]
func (s *Store) CancelJobStatus(ctx context.Context, id string) error {
	now := time.Now().UTC()
	nowText := TimeText(now)

	res, err := s.w.ExecContext(ctx, sqlCancelJobStatus, nowText, nowText, id)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil // successfully transitioned to canceled
	}

	// Zero rows: job was not found or was already terminal.
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err // propagates ErrNotFound
	}
	if job.Status == store.JobStatusCanceled {
		return nil // already canceled — idempotent
	}
	return store.ErrConflict // completed or failed — cannot cancel
}

// DemoteStalledJobs implements [store.JobStore].
//
// It lists the stalled jobs unlocked, then demotes each in its own
// transaction: the job row locked, the stalled predicate checked again under
// the lock, then the write. A lease holds the job row shared, so a lease in
// flight is waited for and the re-check sees the task it assigned; a job that
// is no longer stalled is left alone. On SQLite the re-check is a redundant
// re-read.
//
// The returned IDs are in id order, the order the candidates are locked in. On
// error, the result lists the jobs already demoted: each demotion is its own
// committed transaction.
func (s *Store) DemoteStalledJobs(ctx context.Context, now time.Time) ([]string, error) {
	candidates, err := s.stalledJobCandidates(ctx)
	if err != nil {
		return nil, err
	}
	nowText := TimeText(now.UTC())
	var demoted []string
	for _, id := range candidates {
		var got bool
		err := s.inTx(ctx, "demote stalled job", func(tx *dbTx) (txOutcome, error) {
			got = false
			if err := tx.lockAnchors(ctx, jobAnchor(id)); err != nil {
				return txRollback, err
			}
			var out string
			err := tx.QueryRowContext(ctx, sqlDemoteStalledJob, nowText, id).Scan(&out)
			if errors.Is(err, sql.ErrNoRows) {
				return txRollback, nil
			}
			if err != nil {
				return txRollback, fmt.Errorf("store: demote job %s: %w", id, mapErr(err))
			}
			got = true
			return txCommit, nil
		})
		if err != nil {
			return demoted, err
		}
		if got {
			demoted = append(demoted, id)
		}
	}
	return demoted, nil
}

// stalledJobCandidates lists, in id order, the jobs the stall sweep will
// re-check. The read goes through the write pool so that it sees this store's
// own committed writes. Correctness does not rest on how fresh the list is:
// every job in it is locked and re-checked before it is demoted, so a stale
// entry is skipped and a missing one is found by the next sweep.
func (s *Store) stalledJobCandidates(ctx context.Context) ([]string, error) {
	rows, err := s.w.QueryContext(ctx, sqlStalledJobCandidates)
	if err != nil {
		return nil, fmt.Errorf("store: list stalled jobs: %w", mapErr(err))
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan stalled job id: %w", err)
		}
		ids = append(ids, id)
	}
	// rows.Err is not a wrapper's error: classify it as queryTasksTx does.
	return ids, mapErr(classifyErr(s.d, rows.Err()))
}

// DeleteTerminalJobsBefore implements [store.JobStore]. It selects the eligible
// job IDs first, then deletes each via the shared cascade, all inside one
// transaction so a partially-applied sweep can never leave orphaned child rows.
//
// The SELECT is a snapshot, and a retry can revive one of its jobs before
// that job's turn comes. So each job takes its own job anchor and is then
// re-checked against the full eligibility predicate (terminal status, cutoff,
// no live dependent) immediately before its cascade starts; a job that no
// longer matches is skipped, neither deleted nor reported. On SQLite the
// single write connection means the snapshot cannot go stale, so the re-check
// never skips; it is a PostgreSQL store's guard, where the job-row lock is
// what makes it sound. The sweep holds every lock it takes until its one
// commit, so the candidates are sorted by id before they are anchored: the
// SELECT has no ORDER BY, and an unsorted sweep could take two jobs' locks in
// the opposite order to another writer that locks several jobs in id order.
func (s *Store) DeleteTerminalJobsBefore(
	ctx context.Context, cutoff time.Time, includeFailed bool,
) ([]store.DeletedJob, error) {
	query, args := expiredJobsQuery(cutoff, includeFailed)
	// recheck narrows the same predicate to one job; the appended text is a
	// constant and the id is bound.
	recheck := query + sqlExpiredJobByID

	var deleted []store.DeletedJob
	err := s.inTx(ctx, "delete terminal jobs", func(tx *dbTx) (txOutcome, error) {
		candidates, err := selectExpiredJobsTx(ctx, tx, query, args)
		if err != nil {
			return txRollback, err
		}
		// Lock in id order, so the sweep's locks follow the global order every
		// other job-anchored writer uses.
		slices.SortFunc(candidates, func(a, b store.DeletedJob) int { return cmp.Compare(a.ID, b.ID) })
		deleted = candidates[:0]
		for _, d := range candidates {
			purged, err := purgeExpiredJobTx(ctx, tx, recheck, args, d.ID)
			if err != nil {
				return txRollback, err
			}
			if purged {
				deleted = append(deleted, d)
			}
		}
		return txCommit, nil
	})
	if err != nil {
		return nil, err
	}
	return deleted, nil
}

// expiredJobsQuery builds the retention eligibility SELECT and its bound
// arguments: the statuses the sweep covers, then the cutoff.
func expiredJobsQuery(cutoff time.Time, includeFailed bool) (query string, args []any) {
	statuses := []any{string(store.JobStatusCompleted), string(store.JobStatusCanceled)}
	if includeFailed {
		statuses = append(statuses, string(store.JobStatusFailed))
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	// placeholders is "?,?[,?]" — only ? marks; status values are bound.
	query = sqlSelectExpiredJobsPrefix + placeholders + sqlSelectExpiredJobsSuffix
	return query, slices.Concat(statuses, []any{TimeText(cutoff.UTC())})
}

// selectExpiredJobsTx runs the retention eligibility SELECT inside tx.
func selectExpiredJobsTx(ctx context.Context, tx *dbTx, query string, args []any) ([]store.DeletedJob, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.DeletedJob
	for rows.Next() {
		var d store.DeletedJob
		if err := rows.Scan(&d.ID, &d.Name, &d.FarmID, &d.QueueID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// purgeExpiredJobTx takes the job's anchor, re-checks that it is still
// eligible for retention and, if so, runs the cascade. It reports whether the
// job was deleted. The anchor comes first so that, once the re-check passes, no
// other job-anchored writer can change the job's status before the final
// DELETE; the re-check comes before any child row is touched because the sweep
// is one transaction and a half-purged job cannot be skipped after the fact. As
// for [Store.DeleteJob], the job's logged tasks are locked before the cascade
// ([lockJobLoggedTasksTx]), so a log append waits for the sweep instead of
// landing mid-cascade, and the cascade's foreign-key error stays retryable
// ([cascadeErr]) as a backstop.
func purgeExpiredJobTx(ctx context.Context, tx *dbTx, recheck string, args []any, id string) (bool, error) {
	if err := tx.lockAnchors(ctx, jobAnchor(id)); err != nil {
		return false, err
	}
	var still store.DeletedJob
	err := tx.QueryRowContext(ctx, recheck, slices.Concat(args, []any{id})...).
		Scan(&still.ID, &still.Name, &still.FarmID, &still.QueueID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapErr(err)
	}
	if err := lockJobLoggedTasksTx(ctx, tx, id); err != nil {
		return false, err
	}
	for _, q := range []string{
		sqlDeleteJobCheckouts, sqlDeleteJobTaskLogs, sqlDeleteJobAttempts,
		sqlDeleteJobTasks, sqlDeleteJobSteps, sqlDeleteJobDependencies, sqlDeleteJobRow,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return false, mapErr(cascadeErr(err))
		}
	}
	return true, nil
}

// lockJobLoggedTasksTx locks, until the commit, every task of job id that a
// log line can reference (each task with an attempt), on a database with row
// locks. CreateTaskLog takes no anchor, but its insert's foreign-key check
// takes FOR KEY SHARE on the line's task row, which FOR UPDATE excludes: an
// append from here on waits for the delete and then fails on that foreign key,
// and one already past the check commits before the lock is granted, so the
// cascade's own DELETE of the logs removes it. Either way no log row lands
// mid-cascade, and the cascade does not have to run again.
//
// It locks the task rows, not the attempt rows, because a log insert checks
// its task_id foreign key before its attempt_id one (the triggers fire in
// creation order). Holding the attempts would let an insert hold its task's
// FOR KEY SHARE while it waits on the attempt, and the cascade's DELETE of the
// tasks would then wait on the insert: a deadlock. No new attempt can appear
// meanwhile: a lease holds the job row, which the caller has locked. On SQLite
// it does nothing (the dialect's ForUpdate is "").
func lockJobLoggedTasksTx(ctx context.Context, tx *dbTx, id string) error {
	forUpdate := tx.d.ForUpdate()
	if forUpdate == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, sqlLockJobLoggedTasks+forUpdate, id); err != nil {
		return fmt.Errorf("store: lock job %s's tasks: %w", id, mapErr(err))
	}
	return nil
}

// cascadeErr reclassifies a foreign-key violation inside a job's delete
// cascade as retryable. [lockJobLoggedTasksTx] keeps a log line from landing
// mid-cascade; this is the backstop for a child row inserted after its table
// was cleared all the same, which makes a later DELETE in the cascade fail on
// its foreign key: running the cascade again deletes the new row too, and inTx
// does that for a retryable error. SQLite's single writer never produces the
// error.
func cascadeErr(err error) error {
	if errClass(err) == ErrClassConflict {
		return &classifiedError{class: ErrClassRetry, err: err}
	}
	return err
}

// DeleteJob implements [store.JobStore]. The cascade runs in a single
// transaction; if the jobs-row delete affects zero rows the job did not exist
// and the transaction is rolled back with [store.ErrNotFound].
//
// The job anchor is taken first, which serializes the cascade against the other
// job-anchored writers (cancel, retry, finalize, completion, retention), and
// against leases, which hold the job row FOR SHARE. A log append
// (CreateTaskLog, no transaction, no anchor) is held off by the lock on the
// job's logged tasks taken next ([lockJobLoggedTasksTx]): it waits for the
// delete instead of inserting a child row after its table was cleared. As a
// backstop the store still retries the cascade (claims, logs, attempts, tasks,
// steps, dependency edges, then the job row) if a DELETE in it fails on a
// foreign key: such a violation is reclassified as retryable ([cascadeErr]),
// and the next run, which starts by clearing the logs again, deletes the new
// row too. A deadlock is already retryable. SQLite cannot produce either
// error: its single write connection serializes every writer.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	return s.inTx(ctx, "delete job", func(tx *dbTx) (txOutcome, error) {
		if err := tx.lockAnchors(ctx, jobAnchor(id)); err != nil {
			return txRollback, err
		}
		if err := lockJobLoggedTasksTx(ctx, tx, id); err != nil {
			return txRollback, err
		}

		for _, q := range []string{
			sqlDeleteJobCheckouts,
			sqlDeleteJobTaskLogs,
			sqlDeleteJobAttempts,
			sqlDeleteJobTasks,
			sqlDeleteJobSteps,
			sqlDeleteJobDependencies,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return txRollback, mapErr(cascadeErr(err))
			}
		}

		res, err := tx.ExecContext(ctx, sqlDeleteJobRow, id)
		if err != nil {
			return txRollback, mapErr(cascadeErr(err))
		}
		n, err := res.RowsAffected()
		if err != nil {
			return txRollback, err
		}
		if n == 0 {
			return txRollback, store.ErrNotFound
		}
		return txCommit, nil
	})
}

// ParkJob implements [store.JobStore].
//
// A zero-row UPDATE is ambiguous between "job not found" and "job already
// terminal"; a follow-up GetJob disambiguates the same way [Store.CancelJobStatus]
// does — terminal is a legitimate no-op (nil), missing propagates [store.ErrNotFound].
func (s *Store) ParkJob(ctx context.Context, jobID, reason string, now time.Time) error {
	nowText := TimeText(now.UTC())

	res, err := s.w.ExecContext(ctx, sqlParkJob, reason, nowText, jobID)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil // successfully parked
	}

	// Zero rows: job was not found or was already terminal.
	if _, err := s.GetJob(ctx, jobID); err != nil {
		return err // propagates ErrNotFound
	}
	return nil // already terminal — legitimate no-op
}

// ResumeJob implements [store.JobStore].
//
// A zero-row UPDATE is ambiguous between "job not found" and "job no longer
// paused"; a follow-up GetJob disambiguates the same way [Store.ParkJob] does —
// not-paused is a legitimate no-op (nil), missing propagates [store.ErrNotFound].
func (s *Store) ResumeJob(ctx context.Context, jobID string, now time.Time) error {
	res, err := s.w.ExecContext(ctx, sqlResumeJob, TimeText(now.UTC()), jobID)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil // successfully resumed
	}

	if _, err := s.GetJob(ctx, jobID); err != nil {
		return err // propagates ErrNotFound
	}
	return nil // no longer paused — legitimate no-op
}
