-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Worker-advertised OpenJD EXPR evaluation caps.
--
-- A worker reports the expr.* limits it will enforce at task execution; the
-- scheduler compares them against the server's own openjd.expr_* limits and
-- refuses to dispatch an EXPR job to a worker that is tighter, on which every
-- task of the job would fail.
--
-- Stored as a JSON object in one column, the same shape as gpu_info and tags,
-- so a new dimension needs no migration. '{}' (the default, and what every
-- existing row gets) means "not advertised" and is read as the worker's
-- compiled-in defaults; see store.WorkerExprLimits.
--
-- NOT NULL DEFAULT '{}' matches gpu_info: scanWorker reads this column into a
-- plain string, so a NULL would be a scan error rather than an empty struct.
--
-- The Down migration's ALTER TABLE ... DROP COLUMN requires SQLite >= 3.35.0
-- (the same note 00002, 00008 and 00013 carry). No CHECK constraint and no
-- index on the column: SQLite refuses DROP COLUMN on a
-- column either references, which would make the Down impossible without a
-- full table rebuild.

-- +goose Up
ALTER TABLE workers ADD COLUMN expr_limits TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE workers DROP COLUMN expr_limits;
