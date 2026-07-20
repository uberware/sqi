-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Record which credential backend verifies each account (Phase 3, component
-- C1). 'local' means the stored password hash; 'ldap' means the directory.
-- Existing rows are local by definition, which the DEFAULT supplies.
--
-- Deliberately no CHECK constraint: SQLite refuses ALTER TABLE DROP COLUMN on
-- a column referenced by one, which would make the Down migration below
-- impossible without a full table rebuild. The value is validated in Go.

-- +goose Up
ALTER TABLE users ADD COLUMN auth_source TEXT NOT NULL DEFAULT 'local';

-- +goose Down
ALTER TABLE users DROP COLUMN auth_source;
