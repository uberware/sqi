-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- The worker process's instance ID. A worker sends a fresh one per
-- process in every registration (boot and NATS reconnect alike); a change
-- means the previous process is gone and its in-flight tasks are reclaimed.
-- Empty means unknown: an older worker sends none.
ALTER TABLE workers ADD COLUMN instance_id TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE workers DROP COLUMN instance_id;
