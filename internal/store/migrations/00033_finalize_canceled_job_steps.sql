-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- Finalizes the open steps of jobs that are already terminal (H4a2 §3.3).
-- Until H4a2 a job cancel wrote its tasks and its job row but never its steps,
-- so every job canceled under v0.3.0 or H4a kept open steps, and a later
-- RetryJob revived its tasks to pending under a step nothing would release.
-- The rule is CancelJobExecution's (sqlCancelJobFinalizeSteps in
-- internal/store/sqlite/finalize.go): a pending step, or one with no tasks,
-- is canceled; any other gets FinalizeStep's outcome. Steps of live jobs are
-- left to the scheduler's start-up reconcile, and a step with a task still in
-- flight is never touched. Pure data repair; a healthy database is unchanged.
UPDATE steps
SET    status = CASE
         WHEN status = 'pending' THEN 'canceled'
         WHEN NOT EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id) THEN 'canceled'
         WHEN EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id AND t.status = 'failed')   THEN 'failed'
         WHEN EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id AND t.status = 'canceled') THEN 'canceled'
         ELSE 'completed' END,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE  status NOT IN ('completed', 'failed', 'canceled')
  AND  EXISTS (SELECT 1 FROM jobs j WHERE j.id = steps.job_id
               AND j.status IN ('completed', 'failed', 'canceled'))
  AND  NOT EXISTS (SELECT 1 FROM tasks t WHERE t.step_id = steps.id
                   AND t.status NOT IN ('succeeded', 'failed', 'canceled'));

-- +goose Down

-- A repair is not reversible: which steps were open is not recorded, and
-- reopening them would re-introduce the stranded retry. Down is a no-op.
SELECT 1;
