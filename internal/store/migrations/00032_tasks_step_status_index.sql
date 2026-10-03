-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- Indexes tasks by step and status for step finalization. FinalizeStep
-- (sqlFinalizeStep in internal/store/sqlite/finalize.go) runs on every terminal
-- task report, on the single write connection, and asks whether any task of
-- the step is still in flight. With only tasks(step_id) that check read the
-- step's task rows one by one up to the first in-flight task, so its cost grew
-- with the step's progress. With (step_id, status) it reads the index alone:
-- every in-flight status sorts ahead of 'succeeded', so a step whose finished
-- tasks succeeded answers at once. Only the step's failed and canceled entries
-- (which sort ahead of pending, ready and running) and the step's last report
-- (which must see every task finished) still walk its entries, index entries
-- rather than table rows. The failed and canceled checks of
-- the same statement become equality searches. Startup's ListStuckSteps asks
-- the in-flight question of every open step and uses the index the same way.
-- Pure index; no data change.
CREATE INDEX IF NOT EXISTS tasks_step_status ON tasks (step_id, status);

-- +goose Down

DROP INDEX IF EXISTS tasks_step_status;
