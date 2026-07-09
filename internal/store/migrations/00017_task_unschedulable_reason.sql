-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Add a reason string set when a ready task cannot be satisfied by any online
-- worker (empty = schedulable). An annotation on the task, not a status.

-- +goose Up
ALTER TABLE tasks ADD COLUMN unschedulable_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN unschedulable_reason;
