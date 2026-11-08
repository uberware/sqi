-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- Each succeeded task's measured run time in milliseconds: its latest attempt
-- (highest attempt_number, then greatest id) from started_at to ended_at, when
-- that attempt succeeded with an ended_at no earlier than its started_at, and
-- NULL otherwise. CompleteTaskAttempt stores it when the task succeeds, so the
-- remaining-time read sums it from the index below instead of finding every
-- succeeded task's latest attempt on each read. The backfill applies the same
-- rule to existing rows.
ALTER TABLE tasks ADD COLUMN run_ms INTEGER;
UPDATE tasks SET run_ms = (
    SELECT CASE
               WHEN a.status = 'succeeded' AND a.ended_at IS NOT NULL
                    AND julianday(a.ended_at) >= julianday(a.started_at)
               THEN CAST(ROUND((julianday(a.ended_at) - julianday(a.started_at)) * 86400000.0) AS INTEGER)
           END
    FROM   task_attempts a
    WHERE  a.task_id = tasks.id
    ORDER BY a.attempt_number DESC, a.id DESC
    LIMIT 1)
WHERE status = 'succeeded';

-- run_ms joins tasks_step_status, so the per-step counts and measured sums are
-- read from the index alone. Every lookup the (step_id, status) index served
-- is still a prefix of this one.
DROP INDEX IF EXISTS tasks_step_status;
CREATE INDEX tasks_step_status ON tasks (step_id, status, run_ms);

-- +goose Down
DROP INDEX IF EXISTS tasks_step_status;
CREATE INDEX tasks_step_status ON tasks (step_id, status);
ALTER TABLE tasks DROP COLUMN run_ms;
