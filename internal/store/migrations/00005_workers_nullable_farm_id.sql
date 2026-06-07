-- SPDX-License-Identifier: AGPL-3.0-only
-- Workers without a farm affiliation (farm_id = NULL) are valid.
--
-- The original schema required farm_id NOT NULL REFERENCES farms(id), which
-- prevents registering a worker that has not been assigned to any farm.
-- Unaffiliated workers are useful for testing and for deployments where all
-- workers accept tasks from any farm.  The scheduler already handles the
-- NULL case: ListWorkers with an empty FarmID omits the farm_id filter, so
-- NULL-farm workers appear in global listings but are not matched to
-- farm-specific job queries.
--
-- SQLite does not support ALTER TABLE to change a column constraint, so this
-- migration recreates the workers table without the NOT NULL on farm_id.

-- +goose Up

PRAGMA foreign_keys = OFF;

CREATE TABLE workers_new (
    id               TEXT    NOT NULL PRIMARY KEY,
    farm_id          TEXT    REFERENCES farms (id), -- nullable: optional farm affiliation
    queue_id         TEXT    REFERENCES queues (id), -- nullable: optional queue affinity
    hostname         TEXT    NOT NULL,
    ip_address       TEXT    NOT NULL DEFAULT '',
    compute_location TEXT    NOT NULL DEFAULT 'default',
    os               TEXT    NOT NULL DEFAULT '',
    os_version       TEXT    NOT NULL DEFAULT '',
    cpu_count        INTEGER NOT NULL DEFAULT 0,
    ram_mb           INTEGER NOT NULL DEFAULT 0,
    -- JSON: {vendor, model, vram_mb, count}
    gpu_info         TEXT    NOT NULL DEFAULT '{}',
    -- JSON: arbitrary string -> string capability tags
    tags             TEXT    NOT NULL DEFAULT '{}',
    -- 'online' | 'offline' | 'disabled'
    status           TEXT    NOT NULL DEFAULT 'offline',
    last_heartbeat_at TEXT,
    registered_at    TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL
);

INSERT INTO workers_new SELECT * FROM workers;

DROP TABLE workers;
ALTER TABLE workers_new RENAME TO workers;

CREATE INDEX workers_farm_id       ON workers (farm_id) WHERE farm_id IS NOT NULL;
CREATE INDEX workers_status        ON workers (status);
CREATE INDEX workers_heartbeat     ON workers (last_heartbeat_at);
CREATE INDEX workers_queue_id      ON workers (queue_id) WHERE queue_id IS NOT NULL;

PRAGMA foreign_keys = ON;

-- +goose Down

PRAGMA foreign_keys = OFF;

-- Re-create the original table with NOT NULL on farm_id.
-- Rows with farm_id IS NULL are dropped (they would violate the constraint).
CREATE TABLE workers_old (
    id               TEXT    NOT NULL PRIMARY KEY,
    farm_id          TEXT    NOT NULL REFERENCES farms (id),
    queue_id         TEXT    REFERENCES queues (id),
    hostname         TEXT    NOT NULL,
    ip_address       TEXT    NOT NULL DEFAULT '',
    compute_location TEXT    NOT NULL DEFAULT 'default',
    os               TEXT    NOT NULL DEFAULT '',
    os_version       TEXT    NOT NULL DEFAULT '',
    cpu_count        INTEGER NOT NULL DEFAULT 0,
    ram_mb           INTEGER NOT NULL DEFAULT 0,
    gpu_info         TEXT    NOT NULL DEFAULT '{}',
    tags             TEXT    NOT NULL DEFAULT '{}',
    status           TEXT    NOT NULL DEFAULT 'offline',
    last_heartbeat_at TEXT,
    registered_at    TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL
);

INSERT INTO workers_old SELECT * FROM workers WHERE farm_id IS NOT NULL;

DROP TABLE workers;
ALTER TABLE workers_old RENAME TO workers;

CREATE INDEX workers_farm_id       ON workers (farm_id);
CREATE INDEX workers_status        ON workers (status);
CREATE INDEX workers_heartbeat     ON workers (last_heartbeat_at);
CREATE INDEX workers_queue_id      ON workers (queue_id) WHERE queue_id IS NOT NULL;

PRAGMA foreign_keys = ON;
