-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Enforce case-insensitive uniqueness on usage pool names.

-- +goose Up
-- A usage-pool name is the trailing segment of an OpenJD capability name
-- (amount.worker.usagepool.<name>), which the spec defines as case-insensitive.
-- The scheduler matches pool requirements case-insensitively, so two pools whose
-- names differ only in case would be the same capability and match ambiguously.
-- This NOCASE unique index rejects such a pair at creation; it subsumes the
-- pre-existing case-sensitive UNIQUE (name) constraint carried over from
-- license_pools.
CREATE UNIQUE INDEX usage_pools_name_nocase_unique
    ON usage_pools (name COLLATE NOCASE);

-- +goose Down
DROP INDEX usage_pools_name_nocase_unique;
