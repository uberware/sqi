-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Persist the human-readable reason a task/attempt reached a terminal
-- non-success. task_attempts.message is the per-attempt reason (next to
-- exit_code); tasks.failure_reason is the denormalized latest reason on the
-- task (mirrors unschedulable_reason), set on terminal failed/canceled and
-- cleared on retry.

-- +goose Up
ALTER TABLE task_attempts ADD COLUMN message TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN failure_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN failure_reason;
ALTER TABLE task_attempts DROP COLUMN message;
