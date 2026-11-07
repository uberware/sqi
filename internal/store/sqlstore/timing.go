// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"

	"github.com/uberware/sqi/internal/store"
)

// timingScope is the job filter one timing read applies to all three of its
// queries. jobsWhere filters the jobs table aliased j; tasksJoin and
// tasksWhere restrict tasks aliased t to the same jobs, the queue form by a
// join on jobs rather than a second IN list. All are spliced into SQL text, so
// they must only ever hold "?" marks and literals, never a value: every ID is
// in args, bound once per query.
type timingScope struct {
	jobsWhere  string
	tasksJoin  string
	tasksWhere string
	args       []any
}

const timingNonTerminal = `j.status NOT IN ('completed', 'failed', 'canceled')`

func timingByJobs(jobIDs []string) timingScope {
	timingIn := `(` + qmarks(len(jobIDs)) + `)`
	return timingScope{
		jobsWhere:  `j.id IN ` + timingIn,
		tasksWhere: `t.job_id IN ` + timingIn,
		args:       stringArgs(jobIDs),
	}
}

func timingByQueues(queueIDs []string) timingScope {
	timingIn := `(` + qmarks(len(queueIDs)) + `)`
	return timingScope{
		jobsWhere:  `j.queue_id IN ` + timingIn + ` AND ` + timingNonTerminal,
		tasksJoin:  `JOIN jobs j ON j.id = t.job_id`,
		tasksWhere: `j.queue_id IN ` + timingIn + ` AND ` + timingNonTerminal,
		args:       stringArgs(queueIDs),
	}
}

func stringArgs(ids []string) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// latestAttemptOf restricts the attempt aliased a to its task's latest: the
// highest attempt_number, the rule sqlRunningProgressByJobs uses, and of two
// attempts sharing it the greater id, so a task joins at most one attempt. The
// fake breaks the tie the same way. task_attempts_task_num, which carries id
// after attempt_number, answers the subquery from the index alone.
const latestAttemptOf = `a.id = (SELECT m.id FROM task_attempts m WHERE m.task_id = t.id ORDER BY m.attempt_number DESC, m.id DESC LIMIT 1)`

// JobTimings implements [store.TimingStore].
func (s *Store) JobTimings(ctx context.Context, jobIDs []string) (map[string]store.JobTiming, error) {
	out := make(map[string]store.JobTiming, len(jobIDs))
	if len(jobIDs) == 0 {
		return out, nil
	}
	jobs, err := s.readJobTimings(ctx, timingByJobs(jobIDs))
	if err != nil {
		return nil, err
	}
	for _, jt := range jobs {
		out[jt.JobID] = jt
	}
	return out, nil
}

// QueueJobTimings implements [store.TimingStore]. Jobs are ordered by ID.
func (s *Store) QueueJobTimings(ctx context.Context, queueIDs []string) ([]store.JobTiming, error) {
	if len(queueIDs) == 0 {
		return nil, nil
	}
	return s.readJobTimings(ctx, timingByQueues(queueIDs))
}

// readJobTimings runs the three timing queries over sc: the jobs, the per-step
// counts with measured sums, and the running tasks. The reads are separate
// statements, so a task that changes between them may be counted in one and
// not another; counts for a job the first read did not return are dropped.
func (s *Store) readJobTimings(ctx context.Context, sc timingScope) ([]store.JobTiming, error) {
	jobs, index, err := s.readTimingJobs(ctx, sc)
	if err != nil || len(jobs) == 0 {
		return jobs, err
	}
	if err := s.readTimingSteps(ctx, sc, jobs, index); err != nil {
		return nil, err
	}
	if err := s.readTimingRunning(ctx, sc, jobs, index); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) readTimingJobs(ctx context.Context, sc timingScope) ([]store.JobTiming, map[string]int, error) {
	q := `SELECT j.id, j.queue_id, j.status, j.estimated_seconds FROM jobs j WHERE ` + sc.jobsWhere + ` ORDER BY j.id`
	rows, err := s.r.QueryContext(ctx, q, sc.args...)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	defer rows.Close()
	var jobs []store.JobTiming
	index := map[string]int{}
	for rows.Next() {
		var jt store.JobTiming
		var status string
		var est sql.NullInt64
		if err := rows.Scan(&jt.JobID, &jt.QueueID, &status, &est); err != nil {
			return nil, nil, mapErr(err)
		}
		jt.Status = store.JobStatus(status)
		jt.EstimatedSeconds = nullIntPtr(est)
		index[jt.JobID] = len(jobs)
		jobs = append(jobs, jt)
	}
	return jobs, index, mapErr(rows.Err())
}

// readTimingSteps fills each job's Steps. A succeeded task is measured when its
// latest attempt succeeded with an ended_at; latestAttemptOf picks one attempt
// per task, so the counts are of tasks.
//
// The counts and the measured sums are one query on purpose: it is driven from
// the jobs (tasks by job_id), where a separate measured query filtered on
// t.status = 'succeeded' is planned on SQLite from the tasks status index,
// reading every succeeded task in the database, finished jobs' included.
func (s *Store) readTimingSteps(ctx context.Context, sc timingScope, jobs []store.JobTiming, index map[string]int) error {
	q := `
SELECT t.job_id, t.step_id, t.status, COUNT(*), COUNT(a.ended_at),
       CAST(COALESCE(SUM(` + s.d.EpochSeconds("a.ended_at") + ` - ` + s.d.EpochSeconds("a.started_at") + `), 0) AS DOUBLE PRECISION)
FROM   tasks t ` + sc.tasksJoin + `
LEFT JOIN task_attempts a ON a.task_id = t.id AND t.status = 'succeeded'
      AND a.status = 'succeeded' AND a.ended_at IS NOT NULL AND ` + latestAttemptOf + `
WHERE  ` + sc.tasksWhere + `
GROUP BY t.job_id, t.step_id, t.status
ORDER BY t.job_id, t.step_id`
	rows, err := s.r.QueryContext(ctx, q, sc.args...)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, stepID, status string
		var n, measured int
		var secs float64
		if err := rows.Scan(&jobID, &stepID, &status, &n, &measured, &secs); err != nil {
			return mapErr(err)
		}
		i, ok := index[jobID]
		if !ok {
			continue
		}
		jt := &jobs[i]
		if len(jt.Steps) == 0 || jt.Steps[len(jt.Steps)-1].StepID != stepID {
			jt.Steps = append(jt.Steps, store.StepTiming{StepID: stepID})
		}
		st := &jt.Steps[len(jt.Steps)-1]
		addStepCount(st, store.TaskStatus(status), n)
		st.Measured += measured
		st.MeasuredSeconds += secs
	}
	return mapErr(rows.Err())
}

// addStepCount adds n tasks in status to st.
func addStepCount(st *store.StepTiming, status store.TaskStatus, n int) {
	switch status {
	case store.TaskStatusPending, store.TaskStatusReady, store.TaskStatusAssigned:
		st.NotStarted += n
	case store.TaskStatusRunning:
		st.Running += n
	case store.TaskStatusSucceeded:
		st.Succeeded += n
	case store.TaskStatusFailed:
		st.Failed += n
	case store.TaskStatusCanceled:
		st.Canceled += n
	}
}

// readTimingRunning fills each job's Running from its running tasks' latest
// attempts.
func (s *Store) readTimingRunning(ctx context.Context, sc timingScope, jobs []store.JobTiming, index map[string]int) error {
	q := `
SELECT t.job_id, t.step_id, a.started_at, a.progress
FROM   tasks t ` + sc.tasksJoin + `
JOIN   task_attempts a ON a.task_id = t.id AND ` + latestAttemptOf + `
WHERE  t.status = 'running' AND ` + sc.tasksWhere + `
ORDER BY a.started_at, t.id`
	rows, err := s.r.QueryContext(ctx, q, sc.args...)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, stepID, startedAt string
		var progress sql.NullInt64
		if err := rows.Scan(&jobID, &stepID, &startedAt, &progress); err != nil {
			return mapErr(err)
		}
		i, ok := index[jobID]
		if !ok {
			continue
		}
		at, err := textToTime(startedAt)
		if err != nil {
			return err
		}
		jobs[i].Running = append(jobs[i].Running, store.RunningTiming{
			StepID: stepID, StartedAt: at, Progress: nullIntPtr(progress),
		})
	}
	return mapErr(rows.Err())
}
