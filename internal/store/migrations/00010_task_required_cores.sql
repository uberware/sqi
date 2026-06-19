-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Add required_cores column to tasks table.

-- +goose Up
-- required_cores is the task's declared CPU reservation (amount.worker.vcpu min).
-- NULL means undeclared: the scheduler treats the cost as the running worker's
-- full CPUCount (one such task per worker).
ALTER TABLE tasks ADD COLUMN required_cores INTEGER;

-- +goose Down
ALTER TABLE tasks DROP COLUMN required_cores;
