-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Drop the unused license_pools.product column.
--
-- The column is a free-text label that nothing uses for scheduling or matching:
-- pools are referenced by name, through "amount.worker.usagepool.<name>" (the
-- name 00007 gives the prefix).

-- +goose Up

ALTER TABLE license_pools DROP COLUMN product;

-- +goose Down

ALTER TABLE license_pools ADD COLUMN product TEXT NOT NULL DEFAULT '';
