-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- Whether a job's cross-job dependency hold is still in place. It is set when
-- a job is submitted waiting on an upstream that has not completed and cleared
-- when the dependency sweep releases it, so a held job that is paused resumes
-- to blocked rather than to pending. Every job blocked now is holding.
ALTER TABLE jobs ADD COLUMN dependency_hold BOOLEAN NOT NULL DEFAULT false;
UPDATE jobs SET dependency_hold = true WHERE status = 'blocked';

-- +goose Down
ALTER TABLE jobs DROP COLUMN dependency_hold;
