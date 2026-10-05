-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Long-form markdown documentation for a product.
--
-- Separate from `description` because the two serve incompatible jobs.
-- `description` must fit a picker card and a native Blender EnumProperty
-- tooltip. Markdown in `description` would not help: it adds formatting, not
-- length budget. So the blurb stays short, plain and searchable, and the
-- documentation lives here.
--
-- `readme` is deliberately not searched. With no search over it, no markdown
-- stripper is needed in either TypeScript or Python, and presetlib.IndexEntry
-- needs no readme field, so the remote preset-index format is unchanged.
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
