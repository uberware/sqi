-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Add the worker's self-reported build version.
--
-- Workers report their sqi-worker build version (internal/version.Version) at
-- registration so the web UI can show which version each worker is running.
-- Existing rows default to '' (unknown); the UI omits the field when empty.

-- +goose Up

ALTER TABLE workers ADD COLUMN version TEXT NOT NULL DEFAULT '';

-- +goose Down

-- ALTER TABLE … DROP COLUMN requires SQLite ≥ 3.35.0.
ALTER TABLE workers DROP COLUMN version;
