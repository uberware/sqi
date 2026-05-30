-- SPDX-License-Identifier: AGPL-3.0-only
-- Initial schema for sqi-server.
--
-- Design notes:
--   - All primary keys are TEXT (UUIDs) for portability to PostgreSQL.
--   - JSON columns are TEXT; PostgreSQL migration can promote them to JSONB.
--   - Timestamps are TEXT in ISO 8601 (YYYY-MM-DDTHH:MM:SS.sssZ).
--   - Schema avoids SQLite-specific type affinity quirks to ease future
--     migration to PostgreSQL (tasks 26-27, deferred Phase 4 work).
--   - Sessions are NOT a first-class table in Phase 1. The OpenJD session ID
--     is recorded as session_id on task_attempts for grouping/attribution.
--     A dedicated sessions table should be added when session-reuse scheduling
--     is implemented (see sqi.md §7.4).

-- +goose Up

-- ============================================================
-- farms
-- ============================================================
CREATE TABLE farms (
    id          TEXT        NOT NULL PRIMARY KEY,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    created_at  TEXT        NOT NULL,
    updated_at  TEXT        NOT NULL,

    CONSTRAINT farms_name_unique UNIQUE (name)
);

-- ============================================================
-- queues
-- ============================================================
CREATE TABLE queues (
    id                   TEXT     NOT NULL PRIMARY KEY,
    farm_id              TEXT     NOT NULL REFERENCES farms (id),
    name                 TEXT     NOT NULL,
    description          TEXT     NOT NULL DEFAULT '',
    priority             INTEGER  NOT NULL DEFAULT 0,
    max_concurrent_tasks INTEGER  NOT NULL DEFAULT 0,  -- 0 = unlimited
    paused               INTEGER  NOT NULL DEFAULT 0,  -- 0 = false, 1 = true
    created_at           TEXT     NOT NULL,
    updated_at           TEXT     NOT NULL,

    CONSTRAINT queues_farm_name_unique UNIQUE (farm_id, name)
);

CREATE INDEX queues_farm_id ON queues (farm_id);

-- ============================================================
-- storage_locations
-- ============================================================
CREATE TABLE storage_locations (
    id          TEXT NOT NULL PRIMARY KEY,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL DEFAULT 'filesystem', -- 'filesystem' | 's3'
    description TEXT NOT NULL DEFAULT '',
    -- JSON object: compute_location_name -> root path string
    -- e.g. {"default": "/mnt/nas/shows", "windows_workers": "Z:\\shows"}
    roots       TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,

    CONSTRAINT storage_locations_name_unique UNIQUE (name)
);

-- ============================================================
-- license_pools
-- ============================================================
CREATE TABLE license_pools (
    id             TEXT    NOT NULL PRIMARY KEY,
    name           TEXT    NOT NULL,
    product        TEXT    NOT NULL,
    server_hint    TEXT    NOT NULL DEFAULT '', -- informational; sqi tracks usage itself
    max_concurrent INTEGER NOT NULL DEFAULT 1,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,

    CONSTRAINT license_pools_name_unique UNIQUE (name)
);

-- ============================================================
-- workers
-- ============================================================
CREATE TABLE workers (
    id               TEXT    NOT NULL PRIMARY KEY,
    farm_id          TEXT    NOT NULL REFERENCES farms (id),
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

CREATE INDEX workers_farm_id       ON workers (farm_id);
CREATE INDEX workers_status        ON workers (status);
CREATE INDEX workers_heartbeat     ON workers (last_heartbeat_at);
CREATE INDEX workers_queue_id      ON workers (queue_id) WHERE queue_id IS NOT NULL;

-- ============================================================
-- jobs
-- ============================================================
CREATE TABLE jobs (
    id              TEXT    NOT NULL PRIMARY KEY,
    farm_id         TEXT    NOT NULL REFERENCES farms (id),
    queue_id        TEXT    NOT NULL REFERENCES queues (id),
    name            TEXT    NOT NULL,
    owner           TEXT    NOT NULL DEFAULT '',
    submitter       TEXT    NOT NULL DEFAULT '',
    priority        INTEGER NOT NULL DEFAULT 50,
    -- 'pending' | 'running' | 'paused' | 'completed' | 'failed' | 'canceled'
    status          TEXT    NOT NULL DEFAULT 'pending',
    project         TEXT    NOT NULL DEFAULT '',
    -- verbatim OpenJD template as submitted
    raw_template    TEXT    NOT NULL DEFAULT '',
    -- 'yaml' | 'json'
    template_format TEXT    NOT NULL DEFAULT 'yaml',
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    started_at      TEXT,
    completed_at    TEXT
);

CREATE INDEX jobs_status          ON jobs (status);
CREATE INDEX jobs_queue_id        ON jobs (queue_id);
CREATE INDEX jobs_farm_status     ON jobs (farm_id, status);
CREATE INDEX jobs_owner           ON jobs (owner);

-- ============================================================
-- steps
-- ============================================================
CREATE TABLE steps (
    id          TEXT    NOT NULL PRIMARY KEY,
    job_id      TEXT    NOT NULL REFERENCES jobs (id),
    name        TEXT    NOT NULL,
    -- JSON array of step names this step depends on, e.g. ["render"]
    depends_on  TEXT    NOT NULL DEFAULT '[]',
    -- ordering within the job for deterministic sequencing
    step_order  INTEGER NOT NULL DEFAULT 0,
    -- 'pending' | 'ready' | 'running' | 'completed' | 'failed' | 'canceled'
    status      TEXT    NOT NULL DEFAULT 'pending',
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL,

    CONSTRAINT steps_job_name_unique UNIQUE (job_id, name)
);

CREATE INDEX steps_job_id ON steps (job_id);
CREATE INDEX steps_status  ON steps (status);

-- ============================================================
-- tasks
-- ============================================================
CREATE TABLE tasks (
    id                 TEXT NOT NULL PRIMARY KEY,
    job_id             TEXT NOT NULL REFERENCES jobs (id),   -- denormalized for query efficiency
    step_id            TEXT NOT NULL REFERENCES steps (id),
    name               TEXT NOT NULL,
    -- JSON: parameter name -> value for this specific task instance
    parameters         TEXT NOT NULL DEFAULT '{}',
    -- 'pending' | 'ready' | 'assigned' | 'running' | 'succeeded' | 'failed' | 'canceled'
    status             TEXT NOT NULL DEFAULT 'pending',
    assigned_worker_id TEXT REFERENCES workers (id),
    assigned_at        TEXT,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);

CREATE INDEX tasks_status              ON tasks (status);
CREATE INDEX tasks_job_id              ON tasks (job_id);
CREATE INDEX tasks_step_id             ON tasks (step_id);
CREATE INDEX tasks_assigned_worker_id  ON tasks (assigned_worker_id) WHERE assigned_worker_id IS NOT NULL;

-- ============================================================
-- task_attempts
-- ============================================================
CREATE TABLE task_attempts (
    id             TEXT    NOT NULL PRIMARY KEY,
    task_id        TEXT    NOT NULL REFERENCES tasks (id),
    worker_id      TEXT    NOT NULL REFERENCES workers (id),
    -- OpenJD session ID reported by the worker; groups attempts that shared a session.
    -- See sqi.md §7.4 for the rationale for not persisting sessions as first-class rows.
    session_id     TEXT,
    attempt_number INTEGER NOT NULL DEFAULT 1,
    -- 'running' | 'succeeded' | 'failed' | 'canceled'
    status         TEXT    NOT NULL DEFAULT 'running',
    exit_code      INTEGER,
    started_at     TEXT    NOT NULL,
    ended_at       TEXT,
    created_at     TEXT    NOT NULL
);

CREATE INDEX task_attempts_task_id   ON task_attempts (task_id);
CREATE INDEX task_attempts_worker_id ON task_attempts (worker_id);
CREATE INDEX task_attempts_session_id ON task_attempts (session_id) WHERE session_id IS NOT NULL;

-- ============================================================
-- license_checkouts
-- ============================================================
CREATE TABLE license_checkouts (
    id              TEXT NOT NULL PRIMARY KEY,
    pool_id         TEXT NOT NULL REFERENCES license_pools (id),
    task_attempt_id TEXT NOT NULL REFERENCES task_attempts (id),
    checked_out_at  TEXT NOT NULL,
    -- NULL while checkout is active; set when task reaches a terminal state
    released_at     TEXT,

    CONSTRAINT license_checkouts_attempt_unique UNIQUE (task_attempt_id, pool_id)
);

-- Primary query: count active checkouts for a pool (released_at IS NULL)
CREATE INDEX license_checkouts_pool_active ON license_checkouts (pool_id, released_at);

-- ============================================================
-- audit_log
-- ============================================================
CREATE TABLE audit_log (
    id          TEXT NOT NULL PRIMARY KEY,
    -- e.g. 'job' | 'task' | 'worker' | 'queue' | 'farm' | 'license_pool'
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    -- e.g. 'submitted' | 'canceled' | 'priority_changed' | 'disabled'
    action      TEXT NOT NULL,
    -- authenticated identity that performed the action
    actor       TEXT NOT NULL DEFAULT '',
    -- JSON: action-specific details (before/after values, reason, etc.)
    details     TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL
);

CREATE INDEX audit_log_entity    ON audit_log (entity_type, entity_id);
CREATE INDEX audit_log_created   ON audit_log (created_at);
CREATE INDEX audit_log_actor     ON audit_log (actor);

-- +goose Down

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS license_checkouts;
DROP TABLE IF EXISTS task_attempts;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS steps;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS license_pools;
DROP TABLE IF EXISTS storage_locations;
DROP TABLE IF EXISTS queues;
DROP TABLE IF EXISTS farms;
