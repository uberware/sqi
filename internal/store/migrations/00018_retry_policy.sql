-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Add retry / failure-limit policy columns and genuine-failure counters.
--
-- Nullable policy columns on farms/queues/jobs mean "inherit from the next
-- level up" (resolution: Job -> Queue -> Farm -> server default). The counters
-- (failed_attempts) track GENUINE ran-and-failed attempts only; lost/reclaimed
-- work never touches them. retry_after gates a re-queued task until its backoff
-- delay elapses. park_reason distinguishes an auto-parked job from a manual pause.

-- +goose Up
ALTER TABLE tasks ADD COLUMN failed_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN retry_after TIMESTAMP;

ALTER TABLE jobs ADD COLUMN failed_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN max_attempts INTEGER;
ALTER TABLE jobs ADD COLUMN retry_delay_seconds INTEGER;
ALTER TABLE jobs ADD COLUMN failure_limit INTEGER;
ALTER TABLE jobs ADD COLUMN park_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE queues ADD COLUMN max_attempts INTEGER;
ALTER TABLE queues ADD COLUMN retry_delay_seconds INTEGER;
ALTER TABLE queues ADD COLUMN failure_limit INTEGER;

ALTER TABLE farms ADD COLUMN max_attempts INTEGER;
ALTER TABLE farms ADD COLUMN retry_delay_seconds INTEGER;
ALTER TABLE farms ADD COLUMN failure_limit INTEGER;

-- +goose Down
ALTER TABLE farms DROP COLUMN failure_limit;
ALTER TABLE farms DROP COLUMN retry_delay_seconds;
ALTER TABLE farms DROP COLUMN max_attempts;

ALTER TABLE queues DROP COLUMN failure_limit;
ALTER TABLE queues DROP COLUMN retry_delay_seconds;
ALTER TABLE queues DROP COLUMN max_attempts;

ALTER TABLE jobs DROP COLUMN park_reason;
ALTER TABLE jobs DROP COLUMN failure_limit;
ALTER TABLE jobs DROP COLUMN retry_delay_seconds;
ALTER TABLE jobs DROP COLUMN max_attempts;
ALTER TABLE jobs DROP COLUMN failed_attempts;

ALTER TABLE tasks DROP COLUMN retry_after;
ALTER TABLE tasks DROP COLUMN failed_attempts;
