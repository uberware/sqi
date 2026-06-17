-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Add a human-readable display name to workers.
--
-- Workers self-report a `name` (the `worker.name` config field, default the
-- hostname) so the web UI can distinguish multiple workers running on a single
-- host, which would otherwise all show the same hostname. Existing rows default
-- to '' and the UI falls back to the hostname when the name is empty.

-- +goose Up

ALTER TABLE workers ADD COLUMN name TEXT NOT NULL DEFAULT '';

-- +goose Down

-- ALTER TABLE … DROP COLUMN requires SQLite ≥ 3.35.0.
ALTER TABLE workers DROP COLUMN name;
