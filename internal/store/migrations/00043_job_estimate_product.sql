-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
-- A job's estimated total task run time in seconds, used only until the job
-- has a measured rate, and the product it was submitted from. The product
-- columns exist so estimates from history can group past jobs by product; they
-- are NULL for a raw-template job.
ALTER TABLE jobs ADD COLUMN estimated_seconds INTEGER;
ALTER TABLE jobs ADD COLUMN product_name TEXT;
ALTER TABLE jobs ADD COLUMN product_version TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN product_version;
ALTER TABLE jobs DROP COLUMN product_name;
ALTER TABLE jobs DROP COLUMN estimated_seconds;
