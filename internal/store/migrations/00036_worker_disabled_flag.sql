-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- Moves "disabled" out of workers.status into its own flag. status was both
-- liveness (online/offline, written by registration, deregistration and the
-- heartbeat sweep) and an operator's decision (disabled), so every liveness
-- write had to take care not to overwrite the decision. Now status is liveness
-- only and disabled is written only by the enable/disable endpoints. A
-- disabled row becomes online: the heartbeat sweep takes it offline, and
-- reclaims anything it holds, if its heartbeat is stale.
ALTER TABLE workers ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;

UPDATE workers SET disabled = 1, status = 'online' WHERE status = 'disabled';

-- +goose Down

UPDATE workers SET status = 'disabled' WHERE disabled = 1;

ALTER TABLE workers DROP COLUMN disabled;
