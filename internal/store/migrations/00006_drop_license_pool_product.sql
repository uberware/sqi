-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Drop the unused license_pools.product column.
--
-- The product field was a free-text descriptive label that was never used for
-- scheduling or matching (pools are referenced by name via
-- "amount.worker.usagepool.<name>" (renamed in migration 00007)). It only added
-- an unexplained field to the create/edit UI, so it is removed.

-- +goose Up

ALTER TABLE license_pools DROP COLUMN product;

-- +goose Down

ALTER TABLE license_pools ADD COLUMN product TEXT NOT NULL DEFAULT '';
