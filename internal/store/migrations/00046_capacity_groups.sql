-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- Capacity groups: a named set of machines the server may grow and shrink to
-- follow demand, with the policy for doing so.
--
-- queue_ids and profile are JSON text (a list of queue IDs and a machine
-- profile). The three *_ms columns are durations in milliseconds. farm_id and
-- a worker's capacity_group_id are foreign keys with no ON DELETE action, so a
-- group cannot be left without its farm and cannot be deleted while a worker
-- belongs to it.
--
-- worker_join_tokens.capacity_group_id binds a token to a group (a token goes
-- with its group); worker_credentials.capacity_group_id records the group the
-- credential enrolled into and, like worker_id, is not a foreign
-- key.
CREATE TABLE capacity_groups (
    id                   TEXT NOT NULL PRIMARY KEY,
    farm_id              TEXT NOT NULL REFERENCES farms (id),
    name                 TEXT NOT NULL,
    kind                 TEXT NOT NULL DEFAULT 'named',
    provider             TEXT NOT NULL DEFAULT 'external',
    enabled              INTEGER NOT NULL DEFAULT 1,
    queue_ids            TEXT NOT NULL DEFAULT '[]',
    min_machines         INTEGER NOT NULL DEFAULT 0,
    max_machines         INTEGER NOT NULL DEFAULT 1,
    priority             INTEGER NOT NULL DEFAULT 0,
    max_step_up          INTEGER NOT NULL DEFAULT 1,
    scale_up_cooldown_ms INTEGER NOT NULL DEFAULT 0,
    idle_timeout_ms      INTEGER NOT NULL DEFAULT 600000,
    boot_timeout_ms      INTEGER NOT NULL DEFAULT 600000,
    retire_action        TEXT NOT NULL DEFAULT 'sleep',
    profile              TEXT NOT NULL DEFAULT '{}',
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL,
    CONSTRAINT capacity_groups_name_unique UNIQUE (name)
);
CREATE INDEX capacity_groups_farm_id ON capacity_groups (farm_id);

ALTER TABLE workers ADD COLUMN capacity_group_id TEXT REFERENCES capacity_groups (id);
ALTER TABLE workers ADD COLUMN ephemeral INTEGER NOT NULL DEFAULT 0;
CREATE INDEX workers_capacity_group_id ON workers (capacity_group_id) WHERE capacity_group_id IS NOT NULL;

ALTER TABLE worker_join_tokens ADD COLUMN capacity_group_id TEXT REFERENCES capacity_groups (id) ON DELETE CASCADE;
ALTER TABLE worker_join_tokens ADD COLUMN revoked_at TEXT;
CREATE INDEX worker_join_tokens_capacity_group_id ON worker_join_tokens (capacity_group_id) WHERE capacity_group_id IS NOT NULL;

ALTER TABLE worker_credentials ADD COLUMN capacity_group_id TEXT;

-- +goose Down
ALTER TABLE worker_credentials DROP COLUMN capacity_group_id;

DROP INDEX IF EXISTS worker_join_tokens_capacity_group_id;
ALTER TABLE worker_join_tokens DROP COLUMN revoked_at;
ALTER TABLE worker_join_tokens DROP COLUMN capacity_group_id;

DROP INDEX IF EXISTS workers_capacity_group_id;
ALTER TABLE workers DROP COLUMN ephemeral;
ALTER TABLE workers DROP COLUMN capacity_group_id;

DROP TABLE capacity_groups;
