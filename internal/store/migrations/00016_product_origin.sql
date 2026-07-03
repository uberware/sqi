-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Add origin tracking for products installed from a community preset library.
-- Empty for 'builtin' and 'custom' products.

-- +goose Up
ALTER TABLE products ADD COLUMN origin_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE products ADD COLUMN origin_fingerprint TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE products DROP COLUMN origin_fingerprint;
ALTER TABLE products DROP COLUMN origin_ref;
