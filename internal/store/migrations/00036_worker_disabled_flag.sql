-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- Moves "disabled" out of workers.status into its own flag, so status is
-- liveness only (online/offline, written by registration, deregistration and
-- the heartbeat sweep) and disabled, an operator's decision, is written only
-- by the enable/disable endpoints and no liveness write can overwrite it. A
-- disabled row becomes online: the heartbeat sweep takes it offline, and
-- reclaims anything it holds, if its heartbeat is stale.
ALTER TABLE workers ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;

UPDATE workers SET disabled = 1, status = 'online' WHERE status = 'disabled';

-- +goose Down

UPDATE workers SET status = 'disabled' WHERE disabled = 1;

ALTER TABLE workers DROP COLUMN disabled;
