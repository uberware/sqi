// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"

	"github.com/uberware/sqi/internal/store"
)

// timingScope is the job filter one timing read applies to all three of its
// queries. jobsWhere filters the jobs table aliased j; stepsIn is a subquery
// selecting the IDs of those jobs' steps. Tasks are reached through stepsIn,
// so the task queries search the tasks(step_id, status, run_ms) index a step
// at a time instead of reading every task of a job through tasks(job_id). All
// are spliced into SQL text, so they must only ever hold "?" marks and
// literals, never a value: every ID is in args, bound once per query.
type timingScope struct {
	jobsWhere string
	stepsIn   string
	args      []any
}

const timingNonTerminal = `j.status NOT IN ('completed', 'failed', 'canceled')`

func timingByJobs(jobIDs []string) timingScope {
	timingIn := `(` + qmarks(len(jobIDs)) + `)`
	return timingScope{
		jobsWhere: `j.id IN ` + timingIn,
		stepsIn:   `SELECT s.id FROM steps s WHERE s.job_id IN ` + timingIn,
		args:      stringArgs(jobIDs),
	}
}

func timingByQueues(queueIDs []string) timingScope {
	timingIn := `(` + qmarks(len(queueIDs)) + `)`
	where := `j.queue_id IN ` + timingIn + ` AND ` + timingNonTerminal
	return timingScope{
		jobsWhere: where,
		stepsIn:   `SELECT s.id FROM steps s JOIN jobs j ON j.id = s.job_id WHERE ` + where,
		args:      stringArgs(queueIDs),
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

// timingStepsQuery counts each step's tasks by status and sums the stored run
// times of its measured tasks (tasks.run_ms, set when a task succeeds). It is
// answered from the tasks(step_id, status, run_ms) index alone, one entry per
// task with no attempt lookups, then joined to steps for the job of each
// grouped row.
func timingStepsQuery(sc timingScope) string {
	return `
SELECT s.job_id, g.step_id, g.status, g.n, g.measured, g.ms
FROM  (SELECT t.step_id, t.status, COUNT(*) AS n, COUNT(t.run_ms) AS measured,
              CAST(COALESCE(SUM(t.run_ms), 0) AS BIGINT) AS ms
       FROM   tasks t
       WHERE  t.step_id IN (` + sc.stepsIn + `)
       GROUP BY t.step_id, t.status) g
JOIN   steps s ON s.id = g.step_id
ORDER BY s.job_id, g.step_id`
}

// readTimingSteps fills each job's Steps from [timingStepsQuery]. Only a
// succeeded group's run times count as measured.
func (s *Store) readTimingSteps(ctx context.Context, sc timingScope, jobs []store.JobTiming, index map[string]int) error {
	q := timingStepsQuery(sc)
	rows, err := s.r.QueryContext(ctx, q, sc.args...)
	if err != nil {
		return mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, stepID, status string
		var n, measured int
		var ms int64
		if err := rows.Scan(&jobID, &stepID, &status, &n, &measured, &ms); err != nil {
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
		if store.TaskStatus(status) == store.TaskStatusSucceeded {
			st.Measured += measured
			st.MeasuredSeconds += float64(ms) / 1000
		}
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

// timingRunningQuery reads each running task's latest attempt: an equality
// search on tasks(step_id, status) per step, then one attempt lookup per
// running task.
func timingRunningQuery(sc timingScope) string {
	return `
SELECT t.job_id, t.step_id, a.started_at, a.progress
FROM   tasks t
JOIN   task_attempts a ON a.task_id = t.id AND ` + latestAttemptOf + `
WHERE  t.status = 'running' AND t.step_id IN (` + sc.stepsIn + `)
ORDER BY a.started_at, t.id`
}

// readTimingRunning fills each job's Running from [timingRunningQuery].
func (s *Store) readTimingRunning(ctx context.Context, sc timingScope, jobs []store.JobTiming, index map[string]int) error {
	q := timingRunningQuery(sc)
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
