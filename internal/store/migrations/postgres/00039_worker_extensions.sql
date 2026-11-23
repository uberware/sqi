-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- The worker-enforced OpenJD extensions a worker advertises at registration
-- (SQI_LOG_CHECKS is the only one). The scheduler withholds a job declaring one
-- from a worker that does not list it, so a job whose checks nobody would
-- enforce is never run without them.
--
-- A JSON list in one TEXT column, the same shape as tags and expr_limits. '[]'
-- (the default, and what every existing row gets) reads as "enforces
-- nothing", which is exact for a worker that does not advertise extensions.
ALTER TABLE workers ADD COLUMN extensions TEXT COLLATE "C" NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE workers DROP COLUMN extensions;
