-- SPDX-License-Identifier: AGPL-3.0-only
-- Task 51: add per-farm maximum concurrent task limit.
--
-- max_concurrent_tasks follows the same convention as queues.max_concurrent_tasks:
-- 0 = unlimited (default). The scheduler enforces this limit before assigning a
-- task to any worker in the farm.

-- +goose Up

ALTER TABLE farms ADD COLUMN max_concurrent_tasks INTEGER NOT NULL DEFAULT 0;

-- +goose Down

-- ALTER TABLE … DROP COLUMN requires SQLite ≥ 3.35.0.
-- modernc.org/sqlite bundles a recent SQLite, so this is safe.
ALTER TABLE farms DROP COLUMN max_concurrent_tasks;
