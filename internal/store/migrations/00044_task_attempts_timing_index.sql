-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- Serves the remaining-time reads: each task's latest attempt, the highest
-- attempt_number and of two sharing it the greater id, found from the index
-- alone, with that attempt's status and run time.
--
-- Its leading column is task_id, so it also serves every lookup the older
-- task_attempts_task_id (task_id) index served; that index is dropped here
-- rather than maintained as a strict prefix on every attempt insert.
DROP INDEX IF EXISTS task_attempts_task_id;
CREATE INDEX task_attempts_task_num ON task_attempts (task_id, attempt_number, id, status, started_at, ended_at);

-- +goose Down
DROP INDEX task_attempts_task_num;
CREATE INDEX task_attempts_task_id ON task_attempts (task_id);
