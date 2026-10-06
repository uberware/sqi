-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Queue-scoped OS identity for task isolation. NULL means no isolation, which
-- is what every existing queue gets.
--
-- Deliberately no CHECK constraint: SQLite refuses ALTER TABLE DROP COLUMN on a
-- column referenced by one, which would make the Down migration impossible
-- without a full table rebuild. The value is validated in Go, including the
-- non-configurable privileged-account refusal list.

-- +goose Up
ALTER TABLE queues ADD COLUMN run_as_user TEXT;
ALTER TABLE queues ADD COLUMN run_as_group TEXT;

-- +goose Down
ALTER TABLE queues DROP COLUMN run_as_group;
ALTER TABLE queues DROP COLUMN run_as_user;
