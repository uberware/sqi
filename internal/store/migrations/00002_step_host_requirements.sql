-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Add host_requirements and compute_location to the steps table.
--
-- host_requirements: JSON-serialized store.StepHostRequirements, populated at
-- job submission time from the OpenJD step's hostRequirements block.
-- Used by the scheduler's capability-matching logic (task 50).
--
-- compute_location: TEXT mirror of the "attr.worker.computelocation" attribute
-- requirement extracted during submission.  Stored redundantly so the scheduler
-- can pre-filter workers in SQL before applying in-memory matching, without
-- parsing the full host_requirements JSON every time.

-- +goose Up

ALTER TABLE steps ADD COLUMN host_requirements TEXT NOT NULL DEFAULT 'null';
ALTER TABLE steps ADD COLUMN compute_location  TEXT NOT NULL DEFAULT '';

-- +goose Down

-- ALTER TABLE … DROP COLUMN requires SQLite ≥ 3.35.0.
-- modernc.org/sqlite bundles a recent SQLite, so this is safe.
ALTER TABLE steps DROP COLUMN host_requirements;
ALTER TABLE steps DROP COLUMN compute_location;
