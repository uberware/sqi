-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Cross-job (whole-job) dependencies: a job may wait for other whole jobs to
-- complete before its tasks become schedulable.
--
-- depends_on_job_id deliberately has NO foreign key. If it cascaded on upstream
-- deletion, the edge would vanish and the release check ("every upstream
-- completed") would see zero edges and wrongly release the waiter. Keeping the
-- edge lets the reconciler tell "upstream deleted" (cancel the dependent) from
-- "upstream done" (release it). Upstream existence is enforced at submit time.

-- +goose Up
CREATE TABLE job_dependencies (
    job_id            TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    depends_on_job_id TEXT NOT NULL,
    created_at        TIMESTAMP NOT NULL,
    PRIMARY KEY (job_id, depends_on_job_id)
);
CREATE INDEX job_dependencies_depends_on ON job_dependencies(depends_on_job_id);

-- +goose Down
DROP INDEX job_dependencies_depends_on;
DROP TABLE job_dependencies;
