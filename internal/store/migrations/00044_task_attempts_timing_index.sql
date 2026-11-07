-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- Serves the remaining-time reads: each task's latest attempt, the highest
-- attempt_number and of two sharing it the greater id, found from the index
-- alone, with that attempt's status and run time.
CREATE INDEX task_attempts_task_num ON task_attempts (task_id, attempt_number, id, status, started_at, ended_at);

-- +goose Down
DROP INDEX task_attempts_task_num;
