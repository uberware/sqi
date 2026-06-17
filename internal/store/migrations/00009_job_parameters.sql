-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Add parameters column to jobs table.
--
-- Persists the fully-bound job-parameter values (JSON object) produced by
-- BindJobParameters at submission time, including applied defaults.  The
-- scheduler reads this column at assignment time so workers receive the
-- correct submitted values rather than just template defaults.
--
-- Pre-existing rows default to '{}' (empty JSON object) for backwards
-- compatibility; the scheduler falls back to template defaults when this
-- column is empty.

-- +goose Up

ALTER TABLE jobs ADD COLUMN parameters TEXT NOT NULL DEFAULT '{}';

-- +goose Down

-- ALTER TABLE … DROP COLUMN requires SQLite ≥ 3.35.0.
ALTER TABLE jobs DROP COLUMN parameters;
