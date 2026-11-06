-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- The latest progress snapshot a worker reported for a task attempt: a percent
-- (NULL when the task reports none), a short status line, and the instant the
-- worker stamped it, which orders snapshots so a late or redelivered one never
-- overwrites a newer one. All three are overwritten in place; only the latest
-- is kept, and a new attempt starts with none.
--
-- The text column is status_text rather than message because message already
-- holds the attempt's failure reason.
ALTER TABLE task_attempts ADD COLUMN progress INTEGER;
ALTER TABLE task_attempts ADD COLUMN status_text TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attempts ADD COLUMN progress_at TEXT;

-- +goose Down
ALTER TABLE task_attempts DROP COLUMN progress_at;
ALTER TABLE task_attempts DROP COLUMN status_text;
ALTER TABLE task_attempts DROP COLUMN progress;
