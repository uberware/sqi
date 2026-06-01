-- SPDX-License-Identifier: AGPL-3.0-only
-- Task 59: structured log ingestion with monotonic sequence numbers.
--
-- task_logs stores log chunks published by workers to task.logs.<task>.
-- Each row is one chunk of stdout/stderr output from a running task attempt.
--
-- Retention and pagination notes:
--   - nats_seq (JetStream message sequence from SQI_LOGS) is the stable cursor
--     used by the REST log-tail endpoint.  It is monotonically increasing within
--     the stream and therefore suitable as an offset for forward paging:
--
--       SELECT ... WHERE attempt_id = ? AND nats_seq > ? ORDER BY nats_seq LIMIT ?
--
--   - worker_seq is the worker-assigned sequence number per attempt (starts at 1).
--     It represents the worker's logical ordering and is used to detect gaps or
--     re-ordered delivery.
--
--   - received_at is the server clock time the chunk was persisted, used only
--     for diagnostics.

-- +goose Up

CREATE TABLE task_logs (
    id           TEXT    NOT NULL PRIMARY KEY,
    task_id      TEXT    NOT NULL REFERENCES tasks (id),
    attempt_id   TEXT    NOT NULL REFERENCES task_attempts (id),

    -- worker-assigned sequence number within the attempt (starts at 1)
    worker_seq   INTEGER NOT NULL,

    -- JetStream message sequence from SQI_LOGS; used as pagination cursor
    nats_seq     INTEGER NOT NULL,

    -- worker timestamp when the chunk was captured
    at           TEXT    NOT NULL,

    -- server timestamp when the row was inserted
    received_at  TEXT    NOT NULL,

    -- 'stdout' | 'stderr'
    stream       TEXT    NOT NULL DEFAULT 'stdout',

    -- raw log text content
    data         TEXT    NOT NULL DEFAULT ''
);

-- Fast forward-paging cursor (primary access pattern for log-tail endpoint)
CREATE INDEX task_logs_attempt_nats_seq ON task_logs (attempt_id, nats_seq);

-- Secondary index by task_id for whole-task log queries
CREATE INDEX task_logs_task_id ON task_logs (task_id);

-- +goose Down

DROP INDEX IF EXISTS task_logs_task_id;
DROP INDEX IF EXISTS task_logs_attempt_nats_seq;
DROP TABLE IF EXISTS task_logs;
