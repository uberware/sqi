-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Compute locations: a named, curated registry of where workers run. Populated
-- by admins and auto-registered from worker registrations. The registry is a
-- catalog only; scheduling matches on the raw compute-location string.

-- +goose Up
CREATE TABLE compute_locations (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- +goose Down
DROP TABLE compute_locations;
