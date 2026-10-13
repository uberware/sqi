-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- A log chunk is identified by its attempt and the worker's per-attempt
-- sequence number. Redelivery (a crash between insert and ack, or a replayed
-- JetStream directory) used to store it again under a fresh row ID. Keep the
-- copy received first, then make the pair unique so ingest can skip repeats.
--
-- The DELETE's correlated lookup is quadratic per attempt unless an index
-- covers (attempt_id, worker_seq), and until the unique index exists only
-- task_logs_attempt_nats_seq (attempt_id) does. So build a plain index for the
-- DELETE and drop it again once the unique index replaces it.
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
