-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Long-form markdown documentation for a product.
--
-- Separate from `description`, which must fit a picker card and a native
-- Blender EnumProperty tooltip, so it stays short, plain and searchable and
-- the documentation lives here.
--
-- `readme` is not searched, so no markdown stripper is needed in TypeScript
-- or Python, and presetlib.IndexEntry carries no readme field.
--
-- NOT NULL DEFAULT '' matches `description` and every other late-added string
-- column in this schema (see 00028's note): scanProduct reads it into a plain
-- string, so a NULL would be a scan error rather than an empty value.
--
-- The Down migration's ALTER TABLE ... DROP COLUMN requires SQLite >= 3.35.0,
-- the same note 00002, 00008, 00013, 00026 and 00028 carry.

-- +goose Up
ALTER TABLE products ADD COLUMN readme TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE products DROP COLUMN readme;
