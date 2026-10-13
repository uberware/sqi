-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- The PostgreSQL schema at version 37, written natively. Version N means the
-- same schema on SQLite and PostgreSQL; every later change ships as a pair
-- (internal/store/migrations/NNNNN_x.sql and postgres/NNNNN_x.sql), and the
-- schema-parity test fails a one-sided change.
--
-- Every text column is COLLATE "C" so ordering is byte order, as SQLite's
-- BINARY is, whatever the database's locale. Timestamps are the store's
-- 30-character UTC text (YYYY-MM-DDTHH:MM:SS.nnnnnnnnnZ), held in the ts
-- domain, whose CHECK enforces the fixed width that text ordering relies on.

CREATE DOMAIN ts AS TEXT COLLATE "C"
    CHECK (VALUE ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{9}Z$');

CREATE TABLE farms (
    id                   TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    name                 TEXT COLLATE "C" NOT NULL,
    description          TEXT COLLATE "C" NOT NULL DEFAULT '',
    created_at           ts NOT NULL,
    updated_at           ts NOT NULL,
    max_concurrent_tasks BIGINT NOT NULL DEFAULT 0,
    max_attempts         BIGINT,
    retry_delay_seconds  BIGINT,
    failure_limit        BIGINT,
    CONSTRAINT farms_name_unique UNIQUE (name)
);

CREATE TABLE queues (
    id                   TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    farm_id              TEXT COLLATE "C" NOT NULL REFERENCES farms (id),
    name                 TEXT COLLATE "C" NOT NULL,
    description          TEXT COLLATE "C" NOT NULL DEFAULT '',
    priority             BIGINT NOT NULL DEFAULT 0,
    max_concurrent_tasks BIGINT NOT NULL DEFAULT 0,
    paused               BOOLEAN NOT NULL DEFAULT false,
    created_at           ts NOT NULL,
    updated_at           ts NOT NULL,
    max_attempts         BIGINT,
    retry_delay_seconds  BIGINT,
    failure_limit        BIGINT,
    run_as_user          TEXT COLLATE "C",
    run_as_group         TEXT COLLATE "C",
    CONSTRAINT queues_farm_name_unique UNIQUE (farm_id, name)
);
CREATE INDEX queues_farm_id ON queues (farm_id);

CREATE TABLE users (
    id            TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    username      TEXT COLLATE "C" NOT NULL,
    display_name  TEXT COLLATE "C" NOT NULL DEFAULT '',
    password_hash TEXT COLLATE "C" NOT NULL,
    role          TEXT COLLATE "C" NOT NULL DEFAULT 'user',
    disabled      BOOLEAN NOT NULL DEFAULT false,
    created_at    ts NOT NULL,
    updated_at    ts NOT NULL,
    auth_source   TEXT COLLATE "C" NOT NULL DEFAULT 'local',
    external_id   TEXT COLLATE "C" NOT NULL DEFAULT ''
);
-- SQLite's "username COLLATE NOCASE UNIQUE". Under COLLATE "C", lower() folds
-- ASCII only, exactly as NOCASE does; Dialect.CIEqual compares lower() to
-- lower(), so it uses this index.
CREATE UNIQUE INDEX users_username_lower_unique ON users (lower(username));
CREATE UNIQUE INDEX idx_users_auth_source_external_id ON users (auth_source, external_id) WHERE external_id != '';

CREATE TABLE api_keys (
    id           TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    user_id      TEXT COLLATE "C" NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT COLLATE "C" NOT NULL DEFAULT '',
    token_hash   TEXT COLLATE "C" NOT NULL UNIQUE,
    prefix       TEXT COLLATE "C" NOT NULL,
    expires_at   ts,
    last_used_at ts,
    revoked_at   ts,
    created_at   ts NOT NULL
);
CREATE INDEX api_keys_user ON api_keys (user_id);

CREATE TABLE sessions (
    id         TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    token_hash TEXT COLLATE "C" NOT NULL UNIQUE,
    user_id    TEXT COLLATE "C" NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at ts NOT NULL,
    created_at ts NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE audit_log (
    id          TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    entity_type TEXT COLLATE "C" NOT NULL,
    entity_id   TEXT COLLATE "C" NOT NULL,
    action      TEXT COLLATE "C" NOT NULL,
    actor       TEXT COLLATE "C" NOT NULL DEFAULT '',
    details     TEXT COLLATE "C" NOT NULL DEFAULT '{}',
    created_at  ts NOT NULL
);
CREATE INDEX audit_log_actor   ON audit_log (actor);
CREATE INDEX audit_log_created ON audit_log (created_at);
CREATE INDEX audit_log_entity  ON audit_log (entity_type, entity_id);

CREATE TABLE compute_locations (
    id          TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    name        TEXT COLLATE "C" NOT NULL UNIQUE,
    description TEXT COLLATE "C" NOT NULL DEFAULT '',
    created_at  ts NOT NULL,
    updated_at  ts NOT NULL
);

CREATE TABLE storage_locations (
    id          TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    name        TEXT COLLATE "C" NOT NULL,
    type        TEXT COLLATE "C" NOT NULL DEFAULT 'filesystem',
    description TEXT COLLATE "C" NOT NULL DEFAULT '',
    roots       TEXT COLLATE "C" NOT NULL DEFAULT '{}',
    created_at  ts NOT NULL,
    updated_at  ts NOT NULL,
    CONSTRAINT storage_locations_name_unique UNIQUE (name)
);

CREATE TABLE products (
    id                 TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    name               TEXT COLLATE "C" NOT NULL,
    title              TEXT COLLATE "C" NOT NULL,
    description        TEXT COLLATE "C" NOT NULL DEFAULT '',
    category           TEXT COLLATE "C" NOT NULL DEFAULT '',
    version            TEXT COLLATE "C" NOT NULL DEFAULT '',
    source             TEXT COLLATE "C" NOT NULL,
    template           TEXT COLLATE "C" NOT NULL,
    format             TEXT COLLATE "C" NOT NULL,
    created_at         ts NOT NULL,
    updated_at         ts NOT NULL,
    origin_ref         TEXT COLLATE "C" NOT NULL DEFAULT '',
    origin_fingerprint TEXT COLLATE "C" NOT NULL DEFAULT '',
    readme             TEXT COLLATE "C" NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX products_name_lower_unique ON products (lower(name));

CREATE TABLE usage_pools (
    id             TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    name           TEXT COLLATE "C" NOT NULL,
    server_hint    TEXT COLLATE "C" NOT NULL DEFAULT '',
    max_concurrent BIGINT NOT NULL DEFAULT 1,
    created_at     ts NOT NULL,
    updated_at     ts NOT NULL,
    CONSTRAINT license_pools_name_unique UNIQUE (name)
);
CREATE UNIQUE INDEX usage_pools_name_nocase_unique ON usage_pools (lower(name));

CREATE TABLE workers (
    id                TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    farm_id           TEXT COLLATE "C" REFERENCES farms (id),
    queue_id          TEXT COLLATE "C" REFERENCES queues (id),
    hostname          TEXT COLLATE "C" NOT NULL,
    ip_address        TEXT COLLATE "C" NOT NULL DEFAULT '',
    compute_location  TEXT COLLATE "C" NOT NULL DEFAULT 'default',
    os                TEXT COLLATE "C" NOT NULL DEFAULT '',
    os_version        TEXT COLLATE "C" NOT NULL DEFAULT '',
    cpu_count         BIGINT NOT NULL DEFAULT 0,
    ram_mb            BIGINT NOT NULL DEFAULT 0,
    gpu_info          TEXT COLLATE "C" NOT NULL DEFAULT '{}',
    tags              TEXT COLLATE "C" NOT NULL DEFAULT '{}',
    status            TEXT COLLATE "C" NOT NULL DEFAULT 'offline',
    last_heartbeat_at ts,
    registered_at     ts NOT NULL,
    updated_at        ts NOT NULL,
    name              TEXT COLLATE "C" NOT NULL DEFAULT '',
    version           TEXT COLLATE "C" NOT NULL DEFAULT '',
    expr_limits       TEXT COLLATE "C" NOT NULL DEFAULT '{}',
    arch              TEXT COLLATE "C" NOT NULL DEFAULT '',
    instance_id       TEXT COLLATE "C" NOT NULL DEFAULT '',
    disabled          BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX workers_farm_id   ON workers (farm_id) WHERE farm_id IS NOT NULL;
CREATE INDEX workers_heartbeat ON workers (last_heartbeat_at);
CREATE INDEX workers_queue_id  ON workers (queue_id) WHERE queue_id IS NOT NULL;
CREATE INDEX workers_status    ON workers (status);

CREATE TABLE worker_credentials (
    id           TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    worker_id    TEXT COLLATE "C" NOT NULL,
    public_key   TEXT COLLATE "C" NOT NULL UNIQUE,
    name         TEXT COLLATE "C" NOT NULL DEFAULT '',
    enrolled_at  ts NOT NULL,
    last_seen_at ts,
    revoked_at   ts
);
CREATE UNIQUE INDEX worker_credentials_active ON worker_credentials (worker_id) WHERE revoked_at IS NULL;

CREATE TABLE worker_join_tokens (
    id         TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    token_hash TEXT COLLATE "C" NOT NULL UNIQUE,
    prefix     TEXT COLLATE "C" NOT NULL,
    name       TEXT COLLATE "C" NOT NULL DEFAULT '',
    expires_at ts NOT NULL,
    used_at    ts,
    created_by TEXT COLLATE "C" NOT NULL DEFAULT '',
    created_at ts NOT NULL
);
CREATE INDEX worker_join_tokens_expires ON worker_join_tokens (expires_at);

CREATE TABLE jobs (
    id                  TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    farm_id             TEXT COLLATE "C" NOT NULL REFERENCES farms (id),
    queue_id            TEXT COLLATE "C" NOT NULL REFERENCES queues (id),
    name                TEXT COLLATE "C" NOT NULL,
    owner               TEXT COLLATE "C" NOT NULL DEFAULT '',
    submitter           TEXT COLLATE "C" NOT NULL DEFAULT '',
    priority            BIGINT NOT NULL DEFAULT 50,
    status              TEXT COLLATE "C" NOT NULL DEFAULT 'pending',
    project             TEXT COLLATE "C" NOT NULL DEFAULT '',
    raw_template        TEXT COLLATE "C" NOT NULL DEFAULT '',
    template_format     TEXT COLLATE "C" NOT NULL DEFAULT 'yaml',
    created_at          ts NOT NULL,
    updated_at          ts NOT NULL,
    started_at          ts,
    completed_at        ts,
    parameters          TEXT COLLATE "C" NOT NULL DEFAULT '{}',
    failed_attempts     BIGINT NOT NULL DEFAULT 0,
    max_attempts        BIGINT,
    retry_delay_seconds BIGINT,
    failure_limit       BIGINT,
    park_reason         TEXT COLLATE "C" NOT NULL DEFAULT '',
    declared_extensions TEXT COLLATE "C" NOT NULL DEFAULT ''
);
CREATE INDEX jobs_farm_status ON jobs (farm_id, status);
CREATE INDEX jobs_owner       ON jobs (owner);
CREATE INDEX jobs_queue_id    ON jobs (queue_id);
CREATE INDEX jobs_status      ON jobs (status);

CREATE TABLE job_dependencies (
    job_id            TEXT COLLATE "C" NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    depends_on_job_id TEXT COLLATE "C" NOT NULL,
    created_at        ts NOT NULL,
    PRIMARY KEY (job_id, depends_on_job_id)
);
CREATE INDEX job_dependencies_depends_on ON job_dependencies (depends_on_job_id);

CREATE TABLE steps (
    id                TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    job_id            TEXT COLLATE "C" NOT NULL REFERENCES jobs (id),
    name              TEXT COLLATE "C" NOT NULL,
    depends_on        TEXT COLLATE "C" NOT NULL DEFAULT '[]',
    step_order        BIGINT NOT NULL DEFAULT 0,
    status            TEXT COLLATE "C" NOT NULL DEFAULT 'pending',
    created_at        ts NOT NULL,
    updated_at        ts NOT NULL,
    host_requirements TEXT COLLATE "C" NOT NULL DEFAULT 'null',
    compute_location  TEXT COLLATE "C" NOT NULL DEFAULT '',
    CONSTRAINT steps_job_name_unique UNIQUE (job_id, name)
);
CREATE INDEX steps_job_id ON steps (job_id);
CREATE INDEX steps_status ON steps (status);

CREATE TABLE tasks (
    id                   TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    job_id               TEXT COLLATE "C" NOT NULL REFERENCES jobs (id),
    step_id              TEXT COLLATE "C" NOT NULL REFERENCES steps (id),
    name                 TEXT COLLATE "C" NOT NULL,
    parameters           TEXT COLLATE "C" NOT NULL DEFAULT '{}',
    status               TEXT COLLATE "C" NOT NULL DEFAULT 'pending',
    assigned_worker_id   TEXT COLLATE "C",
    assigned_at          ts,
    created_at           ts NOT NULL,
    updated_at           ts NOT NULL,
    required_cores       BIGINT,
    unschedulable_reason TEXT COLLATE "C" NOT NULL DEFAULT '',
    failed_attempts      BIGINT NOT NULL DEFAULT 0,
    retry_after          ts,
    failure_reason       TEXT COLLATE "C" NOT NULL DEFAULT ''
);
CREATE INDEX tasks_assigned_worker_id ON tasks (assigned_worker_id) WHERE assigned_worker_id IS NOT NULL;
CREATE INDEX tasks_job_id      ON tasks (job_id);
CREATE INDEX tasks_status      ON tasks (status);
CREATE INDEX tasks_step_id     ON tasks (step_id);
CREATE INDEX tasks_step_status ON tasks (step_id, status);

CREATE TABLE task_attempts (
    id             TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    task_id        TEXT COLLATE "C" NOT NULL REFERENCES tasks (id),
    worker_id      TEXT COLLATE "C" NOT NULL,
    session_id     TEXT COLLATE "C",
    attempt_number BIGINT NOT NULL DEFAULT 1,
    status         TEXT COLLATE "C" NOT NULL DEFAULT 'running',
    exit_code      BIGINT,
    started_at     ts NOT NULL,
    ended_at       ts,
    created_at     ts NOT NULL,
    message        TEXT COLLATE "C" NOT NULL DEFAULT ''
);
CREATE INDEX task_attempts_session_id ON task_attempts (session_id) WHERE session_id IS NOT NULL;
CREATE INDEX task_attempts_task_id    ON task_attempts (task_id);
CREATE INDEX task_attempts_worker_id  ON task_attempts (worker_id);

CREATE TABLE task_logs (
    id          TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    task_id     TEXT COLLATE "C" NOT NULL REFERENCES tasks (id),
    attempt_id  TEXT COLLATE "C" NOT NULL REFERENCES task_attempts (id),
    worker_seq  BIGINT NOT NULL,
    nats_seq    BIGINT NOT NULL,
    at          ts NOT NULL,
    received_at ts NOT NULL,
    stream      TEXT COLLATE "C" NOT NULL DEFAULT 'stdout',
    data        TEXT COLLATE "C" NOT NULL DEFAULT ''
);
CREATE INDEX task_logs_attempt_nats_seq ON task_logs (attempt_id, nats_seq);
CREATE INDEX task_logs_task_id ON task_logs (task_id);

CREATE TABLE usage_claims (
    id              TEXT COLLATE "C" NOT NULL PRIMARY KEY,
    pool_id         TEXT COLLATE "C" NOT NULL REFERENCES usage_pools (id),
    task_attempt_id TEXT COLLATE "C" NOT NULL REFERENCES task_attempts (id),
    checked_out_at  ts NOT NULL,
    released_at     ts,
    CONSTRAINT usage_claims_attempt_unique UNIQUE (task_attempt_id, pool_id)
);
CREATE INDEX usage_claims_pool_active ON usage_claims (pool_id, released_at);

-- +goose Down

DROP TABLE usage_claims;
DROP TABLE task_logs;
DROP TABLE task_attempts;
DROP TABLE tasks;
DROP TABLE steps;
DROP TABLE job_dependencies;
DROP TABLE jobs;
DROP TABLE worker_join_tokens;
DROP TABLE worker_credentials;
DROP TABLE workers;
DROP TABLE usage_pools;
DROP TABLE products;
DROP TABLE storage_locations;
DROP TABLE compute_locations;
DROP TABLE audit_log;
DROP TABLE sessions;
DROP TABLE api_keys;
DROP TABLE users;
DROP TABLE queues;
DROP TABLE farms;
DROP DOMAIN ts;
