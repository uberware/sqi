-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Worker-advertised OpenJD EXPR evaluation caps (EXPR sub-project E4d Task 3).
--
-- A worker reports the expr.* limits it will enforce at task execution; the
-- scheduler compares them against the server's own openjd.expr_* limits and
-- refuses to dispatch an EXPR job to a worker that is tighter, instead of
-- accepting the job and failing every task of it on that host.
--
-- Stored as a JSON object in one column, the same shape as gpu_info and tags,
-- so a later dimension needs no further migration. '{}' (the default, and what
-- every pre-existing row gets) means "not advertised" and is read as the
-- worker's compiled-in defaults — see store.WorkerExprLimits.
--
-- NOT NULL DEFAULT '{}' matches gpu_info: scanWorker reads this column into a
-- plain string, so a NULL would be a scan error rather than an empty struct.

-- +goose Up
ALTER TABLE workers ADD COLUMN expr_limits TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE workers DROP COLUMN expr_limits;
