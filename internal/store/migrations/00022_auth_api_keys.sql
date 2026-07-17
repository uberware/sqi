-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Long-lived API keys (Phase 3, component A2). Machine/SDK credential owned by
-- a user; the raw key is never stored, only its SHA-256 hash. The table exists
-- regardless of auth.enabled and is simply unused when auth is disabled.

-- +goose Up
CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    token_hash   TEXT NOT NULL UNIQUE,
    prefix       TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked_at   TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX api_keys_user ON api_keys(user_id);

-- +goose Down
DROP INDEX api_keys_user;
DROP TABLE api_keys;
