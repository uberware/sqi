-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Products: named, versioned wrappers over OpenJD templates. Built-ins are
-- embedded in the binary and never stored here; this table holds only
-- 'custom' and 'installed' products.

-- +goose Up
CREATE TABLE products (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    category    TEXT NOT NULL DEFAULT '',
    version     TEXT NOT NULL DEFAULT '',
    source      TEXT NOT NULL,
    template    TEXT NOT NULL,
    format      TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- +goose Down
DROP TABLE products;
