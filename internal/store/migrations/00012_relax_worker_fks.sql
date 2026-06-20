-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Relax the foreign keys from tasks.assigned_worker_id and task_attempts.worker_id
-- to workers(id) so that a worker row can be hard-deleted.
--
-- Ephemeral cloud workers spin up, run work, and are destroyed; their rows must
-- be removable (manually or by the offline-retention sweep) without unbounded
-- growth. Both columns are only ever written as snapshots — neither is JOINed
-- back to workers (the UI resolves a task's worker via a separate lookup and
-- falls back to the raw id), so dropping the constraint loses nothing but the
-- delete-time integrity check. worker_id keeps its NOT NULL; assigned_worker_id
-- stays nullable.
--
-- SQLite cannot drop a column constraint in place, so each table is rebuilt
-- (the canonical 12-step table-rebuild). The new DDL mirrors the cumulative
-- current schema: tasks includes required_cores (added in 00010).
--
-- This migration is marked "NO TRANSACTION" so it runs outside goose's wrapping
-- transaction. That is required because "PRAGMA foreign_keys = OFF" is a no-op
-- inside a transaction: on a populated database the implicit row-delete that
-- "DROP TABLE tasks" performs would otherwise fire the foreign keys that
-- task_attempts and task_logs hold on tasks (and task_logs on task_attempts) and
-- fail. With foreign keys genuinely disabled for the rebuild the drops succeed,
-- and re-enabling them at the end does not re-validate existing rows. The
-- operations are deterministic; a failure would halt startup with the version
-- unchanged.

-- +goose NO TRANSACTION

-- +goose Up

PRAGMA foreign_keys = OFF;

-- ── tasks ───────────────────────────────────────────────────────────────────
CREATE TABLE tasks_new (
    id                 TEXT NOT NULL PRIMARY KEY,
    job_id             TEXT NOT NULL REFERENCES jobs (id),   -- denormalized for query efficiency
    step_id            TEXT NOT NULL REFERENCES steps (id),
    name               TEXT NOT NULL,
    parameters         TEXT NOT NULL DEFAULT '{}',
    status             TEXT NOT NULL DEFAULT 'pending',
    assigned_worker_id TEXT,                                 -- snapshot; FK to workers(id) removed
    assigned_at        TEXT,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL,
    required_cores     INTEGER
);

INSERT INTO tasks_new (id, job_id, step_id, name, parameters, status,
                       assigned_worker_id, assigned_at, created_at, updated_at, required_cores)
SELECT id, job_id, step_id, name, parameters, status,
       assigned_worker_id, assigned_at, created_at, updated_at, required_cores
FROM tasks;

DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;

CREATE INDEX tasks_status              ON tasks (status);
CREATE INDEX tasks_job_id              ON tasks (job_id);
CREATE INDEX tasks_step_id             ON tasks (step_id);
CREATE INDEX tasks_assigned_worker_id  ON tasks (assigned_worker_id) WHERE assigned_worker_id IS NOT NULL;

-- ── task_attempts ───────────────────────────────────────────────────────────
CREATE TABLE task_attempts_new (
    id             TEXT    NOT NULL PRIMARY KEY,
    task_id        TEXT    NOT NULL REFERENCES tasks (id),
    worker_id      TEXT    NOT NULL,                          -- snapshot; FK to workers(id) removed
    session_id     TEXT,
    attempt_number INTEGER NOT NULL DEFAULT 1,
    status         TEXT    NOT NULL DEFAULT 'running',
    exit_code      INTEGER,
    started_at     TEXT    NOT NULL,
    ended_at       TEXT,
    created_at     TEXT    NOT NULL
);

INSERT INTO task_attempts_new (id, task_id, worker_id, session_id, attempt_number,
                               status, exit_code, started_at, ended_at, created_at)
SELECT id, task_id, worker_id, session_id, attempt_number,
       status, exit_code, started_at, ended_at, created_at
FROM task_attempts;

DROP TABLE task_attempts;
ALTER TABLE task_attempts_new RENAME TO task_attempts;

CREATE INDEX task_attempts_task_id    ON task_attempts (task_id);
CREATE INDEX task_attempts_worker_id  ON task_attempts (worker_id);
CREATE INDEX task_attempts_session_id ON task_attempts (session_id) WHERE session_id IS NOT NULL;

PRAGMA foreign_keys = ON;

-- +goose Down

PRAGMA foreign_keys = OFF;

-- ── tasks: restore REFERENCES workers(id) ───────────────────────────────────
CREATE TABLE tasks_old (
    id                 TEXT NOT NULL PRIMARY KEY,
    job_id             TEXT NOT NULL REFERENCES jobs (id),
    step_id            TEXT NOT NULL REFERENCES steps (id),
    name               TEXT NOT NULL,
    parameters         TEXT NOT NULL DEFAULT '{}',
    status             TEXT NOT NULL DEFAULT 'pending',
    assigned_worker_id TEXT REFERENCES workers (id),
    assigned_at        TEXT,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL,
    required_cores     INTEGER
);

INSERT INTO tasks_old (id, job_id, step_id, name, parameters, status,
                       assigned_worker_id, assigned_at, created_at, updated_at, required_cores)
SELECT id, job_id, step_id, name, parameters, status,
       assigned_worker_id, assigned_at, created_at, updated_at, required_cores
FROM tasks;

DROP TABLE tasks;
ALTER TABLE tasks_old RENAME TO tasks;

CREATE INDEX tasks_status              ON tasks (status);
CREATE INDEX tasks_job_id              ON tasks (job_id);
CREATE INDEX tasks_step_id             ON tasks (step_id);
CREATE INDEX tasks_assigned_worker_id  ON tasks (assigned_worker_id) WHERE assigned_worker_id IS NOT NULL;

-- ── task_attempts: restore REFERENCES workers(id) ───────────────────────────
CREATE TABLE task_attempts_old (
    id             TEXT    NOT NULL PRIMARY KEY,
    task_id        TEXT    NOT NULL REFERENCES tasks (id),
    worker_id      TEXT    NOT NULL REFERENCES workers (id),
    session_id     TEXT,
    attempt_number INTEGER NOT NULL DEFAULT 1,
    status         TEXT    NOT NULL DEFAULT 'running',
    exit_code      INTEGER,
    started_at     TEXT    NOT NULL,
    ended_at       TEXT,
    created_at     TEXT    NOT NULL
);

INSERT INTO task_attempts_old (id, task_id, worker_id, session_id, attempt_number,
                               status, exit_code, started_at, ended_at, created_at)
SELECT id, task_id, worker_id, session_id, attempt_number,
       status, exit_code, started_at, ended_at, created_at
FROM task_attempts;

DROP TABLE task_attempts;
ALTER TABLE task_attempts_old RENAME TO task_attempts;

CREATE INDEX task_attempts_task_id    ON task_attempts (task_id);
CREATE INDEX task_attempts_worker_id  ON task_attempts (worker_id);
CREATE INDEX task_attempts_session_id ON task_attempts (session_id) WHERE session_id IS NOT NULL;

PRAGMA foreign_keys = ON;
