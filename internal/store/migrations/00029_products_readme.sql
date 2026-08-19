-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Long-form markdown documentation for a product.
--
-- Separate from `description` because the two serve incompatible jobs.
-- `description` must fit a picker card and a native Blender EnumProperty
-- tooltip, which is why commit a1e529e shortened every shipped preset's
-- description by hand -- the longest, 940 characters on
-- ffmpeg-segment-transcode-expr, was documentation wearing a blurb's clothes.
-- Markdown in `description` would not have helped: it adds formatting, not
-- length budget. So the blurb stays short, plain and searchable, and the
-- documentation moves here.
--
-- `readme` is deliberately NOT searched. That is what keeps the change small:
-- with no search over it, no markdown stripper is needed in either TypeScript
-- or Python, and presetlib.IndexEntry needs no readme field, so the remote
-- preset-index format is unchanged.
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
