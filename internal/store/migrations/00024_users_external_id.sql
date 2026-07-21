-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Stable per-source identity for externally-authenticated accounts (Phase 3,
-- component C2). Holds the OIDC 'sub' claim, or the directory attribute named
-- by auth.ldap.unique_id_attr (objectGUID on AD, entryUUID elsewhere).
--
-- Accounts are matched on (auth_source, external_id), never on username.
-- Matching on a name lets a recycled email address inherit a departed user's
-- account, and lets a rename at the provider orphan one — both silent.
--
-- Empty string, not NULL, so scanUser needs no sql.NullString. Local accounts
-- carry '' and are excluded from the unique index by its WHERE clause.
--
-- Deliberately no CHECK constraint, for the same reason 00023 has none:
-- SQLite refuses ALTER TABLE DROP COLUMN on a column referenced by one, which
-- would make the Down migration impossible without a full table rebuild.

-- +goose Up
ALTER TABLE users ADD COLUMN external_id TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX idx_users_auth_source_external_id
  ON users (auth_source, external_id)
  WHERE external_id != '';

-- +goose Down
DROP INDEX idx_users_auth_source_external_id;
ALTER TABLE users DROP COLUMN external_id;
