-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Local user accounts and server-side sessions (Phase 3, component A1).
-- Both tables exist regardless of auth.enabled; they are simply unused when
-- auth is disabled. Roles are stored but not enforced until B1.

-- +goose Up
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL COLLATE NOCASE UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user',
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

-- +goose Down
DROP INDEX sessions_user;
DROP TABLE sessions;
DROP TABLE users;
