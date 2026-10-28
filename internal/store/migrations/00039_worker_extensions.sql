-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- The worker-enforced OpenJD extensions a worker advertises at registration
-- (today only SQI_LOG_CHECKS). The scheduler withholds a job declaring one from
-- a worker that does not list it, so a job whose checks nobody would enforce is
-- never run without them.
--
-- A JSON list in one TEXT column, the same shape as tags and expr_limits. '[]'
-- (the default, and what every pre-existing row gets) reads as "enforces
-- nothing", which is exact: no sqi-worker built before this column enforced any
-- extension.
ALTER TABLE workers ADD COLUMN extensions TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE workers DROP COLUMN extensions;
