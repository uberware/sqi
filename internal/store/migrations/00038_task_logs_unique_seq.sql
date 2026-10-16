-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- A log chunk is identified by its attempt and the worker's per-attempt
-- sequence number. Without a unique index, redelivery (a crash between insert
-- and ack, or a replayed JetStream directory) stores a chunk again under a
-- fresh row ID. Keep the copy received first, then make the pair unique so
-- ingest can skip repeats.
--
-- The DELETE's correlated lookup is quadratic per attempt unless an index
-- covers (attempt_id, worker_seq), so a temporary plain index serves it and is
-- dropped before the unique index is built.
CREATE INDEX task_logs_dedupe_00038 ON task_logs (attempt_id, worker_seq);
DELETE FROM task_logs
WHERE EXISTS (
    SELECT 1 FROM task_logs d
    WHERE d.attempt_id = task_logs.attempt_id
      AND d.worker_seq = task_logs.worker_seq
      AND (d.received_at < task_logs.received_at
           OR (d.received_at = task_logs.received_at AND d.id < task_logs.id))
);
DROP INDEX task_logs_dedupe_00038;
CREATE UNIQUE INDEX task_logs_attempt_worker_seq ON task_logs (attempt_id, worker_seq);

-- +goose Down
DROP INDEX task_logs_attempt_worker_seq;
