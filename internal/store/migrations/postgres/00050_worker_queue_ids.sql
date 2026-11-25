-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- workers.queue_ids replaces queue_id and holds the worker's whole queue list:
-- a JSON array of queue IDs, '[]' for a worker that serves every queue in its
-- farm. It has no foreign key: an ID whose queue was deleted, or never existed,
-- stays in the list and matches no job, so the worker is leased less, never
-- more. A worker restricted to one queue keeps it as a one-element list. Queue
-- IDs are server-generated UUIDs, so building the JSON text by concatenation
-- needs no escaping, and it is the same text the store writes.
ALTER TABLE workers ADD COLUMN queue_ids TEXT COLLATE "C" NOT NULL DEFAULT '[]';
UPDATE workers SET queue_ids = '["' || queue_id || '"]' WHERE queue_id IS NOT NULL;
DROP INDEX workers_queue_id;
ALTER TABLE workers DROP COLUMN queue_id;

-- +goose Down
-- Only the first listed queue survives a downgrade: the older schema holds one.
ALTER TABLE workers ADD COLUMN queue_id TEXT COLLATE "C" REFERENCES queues (id);
UPDATE workers SET queue_id = queue_ids::json ->> 0
WHERE queue_ids <> '[]' AND (queue_ids::json ->> 0) IN (SELECT id FROM queues);
CREATE INDEX workers_queue_id  ON workers (queue_id) WHERE queue_id IS NOT NULL;
ALTER TABLE workers DROP COLUMN queue_ids;
