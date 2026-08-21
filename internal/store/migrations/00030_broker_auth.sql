-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- worker_credentials binds a worker's self-chosen ID to an Ed25519 nkey
-- public key. worker_id is deliberately NOT a foreign key to workers(id): a
-- credential is issued before the worker has ever registered, and on
-- auth-off farms worker rows exist with no credential at all.
CREATE TABLE worker_credentials (
    id           TEXT PRIMARY KEY,
    worker_id    TEXT NOT NULL UNIQUE,
    public_key   TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL DEFAULT '',
    enrolled_at  TEXT NOT NULL,
    last_seen_at TEXT,
    revoked_at   TEXT
);
CREATE INDEX worker_credentials_active ON worker_credentials (worker_id) WHERE revoked_at IS NULL;

-- worker_join_tokens mirrors api_keys (00022): only the hash is stored, and
-- the prefix exists so an operator can identify a token in a list without
-- the server ever holding the secret.
CREATE TABLE worker_join_tokens (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    prefix     TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX worker_join_tokens_expires ON worker_join_tokens (expires_at);

-- +goose Down
DROP INDEX worker_join_tokens_expires;
DROP TABLE worker_join_tokens;
DROP INDEX worker_credentials_active;
DROP TABLE worker_credentials;
