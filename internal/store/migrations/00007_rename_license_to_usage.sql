-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Rename the license_pools / license_checkouts tables to usage_pools /
-- usage_claims. The feature is a generic named concurrency limit; software
-- licenses are one use among others (product, version, show). No behavior
-- change. Data is preserved.
--
-- The pools table is renamed in place. The claims table is recreated (rather
-- than renamed in place) so its foreign key explicitly targets usage_pools and
-- its constraint/index names follow the new naming — SQLite's rewriting of FK
-- references on a bare RENAME is version/PRAGMA dependent, so we copy.

-- +goose Up

ALTER TABLE license_pools RENAME TO usage_pools;

CREATE TABLE usage_claims (
    id              TEXT NOT NULL PRIMARY KEY,
    pool_id         TEXT NOT NULL REFERENCES usage_pools (id),
    task_attempt_id TEXT NOT NULL REFERENCES task_attempts (id),
    checked_out_at  TEXT NOT NULL,
    released_at     TEXT,

    CONSTRAINT usage_claims_attempt_unique UNIQUE (task_attempt_id, pool_id)
);

INSERT INTO usage_claims (id, pool_id, task_attempt_id, checked_out_at, released_at)
    SELECT id, pool_id, task_attempt_id, checked_out_at, released_at
    FROM   license_checkouts;

DROP TABLE license_checkouts;

CREATE INDEX usage_claims_pool_active ON usage_claims (pool_id, released_at);

-- Rewrite the JSON stored in steps.host_requirements so that:
--   "license_pools" key  →  "usage_pools"
--   amount.worker.licensepool.  →  amount.worker.usagepool.
-- Without this, existing step rows would silently lose their pool
-- requirements because the Go code now reads the new key/prefix.
UPDATE steps
SET host_requirements = replace(
      replace(host_requirements, '"license_pools"', '"usage_pools"'),
      'amount.worker.licensepool.',
      'amount.worker.usagepool.'
    )
WHERE host_requirements LIKE '%"license_pools"%'
   OR host_requirements LIKE '%amount.worker.licensepool.%';

-- +goose Down

-- Reverse the steps.host_requirements JSON rewrite before renaming tables back.
UPDATE steps
SET host_requirements = replace(
      replace(host_requirements, '"usage_pools"', '"license_pools"'),
      'amount.worker.usagepool.',
      'amount.worker.licensepool.'
    )
WHERE host_requirements LIKE '%"usage_pools"%'
   OR host_requirements LIKE '%amount.worker.usagepool.%';

ALTER TABLE usage_pools RENAME TO license_pools;

CREATE TABLE license_checkouts (
    id              TEXT NOT NULL PRIMARY KEY,
    pool_id         TEXT NOT NULL REFERENCES license_pools (id),
    task_attempt_id TEXT NOT NULL REFERENCES task_attempts (id),
    checked_out_at  TEXT NOT NULL,
    released_at     TEXT,

    CONSTRAINT license_checkouts_attempt_unique UNIQUE (task_attempt_id, pool_id)
);

INSERT INTO license_checkouts (id, pool_id, task_attempt_id, checked_out_at, released_at)
    SELECT id, pool_id, task_attempt_id, checked_out_at, released_at
    FROM   usage_claims;

DROP TABLE usage_claims;

CREATE INDEX license_checkouts_pool_active ON license_checkouts (pool_id, released_at);
