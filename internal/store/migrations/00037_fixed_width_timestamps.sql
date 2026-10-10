-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- Rewrites every stored timestamp to the one form the store now writes:
-- RFC 3339 in UTC with all nine fractional digits, 30 characters
-- (YYYY-MM-DDTHH:MM:SS.nnnnnnnnnZ; sqlite.timeLayout). SQLite compares and
-- orders these columns as TEXT, and earlier releases stored three forms that
-- sort wrong against each other within one second: trimmed RFC3339Nano
-- ("…:05Z" sorts after "…:05.5Z"), three digits from the repair migrations'
-- strftime, and nine digits from submission's row clock.
--
-- Two shapes are recognized: "…SSZ" gains ".000000000", and "…SS.<1-8
-- digits>Z" has its fraction padded with zeros to nine. Both keep the same
-- instant. Anything else (NULL, a value of any other shape) is left exactly as
-- it was, and a 30-character value is not touched, so applying this twice
-- changes nothing. One UPDATE per table, so a table is scanned once.

UPDATE api_keys SET
    expires_at = CASE
        WHEN expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(expires_at) BETWEEN 22 AND 29
              AND substr(expires_at, 21, length(expires_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(expires_at, 1, 19) || '.' || substr(substr(expires_at, 21, max(length(expires_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE expires_at END,
    last_used_at = CASE
        WHEN last_used_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (last_used_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(last_used_at) BETWEEN 22 AND 29
              AND substr(last_used_at, 21, length(last_used_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(last_used_at, 1, 19) || '.' || substr(substr(last_used_at, 21, max(length(last_used_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE last_used_at END,
    revoked_at = CASE
        WHEN revoked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (revoked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(revoked_at) BETWEEN 22 AND 29
              AND substr(revoked_at, 21, length(revoked_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(revoked_at, 1, 19) || '.' || substr(substr(revoked_at, 21, max(length(revoked_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE revoked_at END,
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END
WHERE length(expires_at) <> 30 OR length(last_used_at) <> 30 OR length(revoked_at) <> 30 OR length(created_at) <> 30;

UPDATE audit_log SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END
WHERE length(created_at) <> 30;

UPDATE compute_locations SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30;

UPDATE farms SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30;

UPDATE job_dependencies SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END
WHERE length(created_at) <> 30;

UPDATE jobs SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END,
    started_at = CASE
        WHEN started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(started_at) BETWEEN 22 AND 29
              AND substr(started_at, 21, length(started_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(started_at, 1, 19) || '.' || substr(substr(started_at, 21, max(length(started_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE started_at END,
    completed_at = CASE
        WHEN completed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (completed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(completed_at) BETWEEN 22 AND 29
              AND substr(completed_at, 21, length(completed_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(completed_at, 1, 19) || '.' || substr(substr(completed_at, 21, max(length(completed_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE completed_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30 OR length(started_at) <> 30 OR length(completed_at) <> 30;

UPDATE products SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30;

UPDATE queues SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30;

UPDATE sessions SET
    expires_at = CASE
        WHEN expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(expires_at) BETWEEN 22 AND 29
              AND substr(expires_at, 21, length(expires_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(expires_at, 1, 19) || '.' || substr(substr(expires_at, 21, max(length(expires_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE expires_at END,
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END
WHERE length(expires_at) <> 30 OR length(created_at) <> 30;

UPDATE steps SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30;

UPDATE storage_locations SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30;

UPDATE task_attempts SET
    started_at = CASE
        WHEN started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(started_at) BETWEEN 22 AND 29
              AND substr(started_at, 21, length(started_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(started_at, 1, 19) || '.' || substr(substr(started_at, 21, max(length(started_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE started_at END,
    ended_at = CASE
        WHEN ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(ended_at) BETWEEN 22 AND 29
              AND substr(ended_at, 21, length(ended_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(ended_at, 1, 19) || '.' || substr(substr(ended_at, 21, max(length(ended_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE ended_at END,
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END
WHERE length(started_at) <> 30 OR length(ended_at) <> 30 OR length(created_at) <> 30;

UPDATE task_logs SET
    at = CASE
        WHEN at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(at) BETWEEN 22 AND 29
              AND substr(at, 21, length(at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(at, 1, 19) || '.' || substr(substr(at, 21, max(length(at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE at END,
    received_at = CASE
        WHEN received_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (received_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(received_at) BETWEEN 22 AND 29
              AND substr(received_at, 21, length(received_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(received_at, 1, 19) || '.' || substr(substr(received_at, 21, max(length(received_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE received_at END
WHERE length(at) <> 30 OR length(received_at) <> 30;

UPDATE tasks SET
    assigned_at = CASE
        WHEN assigned_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (assigned_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(assigned_at) BETWEEN 22 AND 29
              AND substr(assigned_at, 21, length(assigned_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(assigned_at, 1, 19) || '.' || substr(substr(assigned_at, 21, max(length(assigned_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE assigned_at END,
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END,
    retry_after = CASE
        WHEN retry_after GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (retry_after GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(retry_after) BETWEEN 22 AND 29
              AND substr(retry_after, 21, length(retry_after) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(retry_after, 1, 19) || '.' || substr(substr(retry_after, 21, max(length(retry_after) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE retry_after END
WHERE length(assigned_at) <> 30 OR length(created_at) <> 30 OR length(updated_at) <> 30 OR length(retry_after) <> 30;

UPDATE usage_claims SET
    checked_out_at = CASE
        WHEN checked_out_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (checked_out_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(checked_out_at) BETWEEN 22 AND 29
              AND substr(checked_out_at, 21, length(checked_out_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(checked_out_at, 1, 19) || '.' || substr(substr(checked_out_at, 21, max(length(checked_out_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE checked_out_at END,
    released_at = CASE
        WHEN released_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (released_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(released_at) BETWEEN 22 AND 29
              AND substr(released_at, 21, length(released_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(released_at, 1, 19) || '.' || substr(substr(released_at, 21, max(length(released_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE released_at END
WHERE length(checked_out_at) <> 30 OR length(released_at) <> 30;

UPDATE usage_pools SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30;

UPDATE users SET
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(created_at) <> 30 OR length(updated_at) <> 30;

UPDATE worker_credentials SET
    enrolled_at = CASE
        WHEN enrolled_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (enrolled_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(enrolled_at) BETWEEN 22 AND 29
              AND substr(enrolled_at, 21, length(enrolled_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(enrolled_at, 1, 19) || '.' || substr(substr(enrolled_at, 21, max(length(enrolled_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE enrolled_at END,
    last_seen_at = CASE
        WHEN last_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (last_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(last_seen_at) BETWEEN 22 AND 29
              AND substr(last_seen_at, 21, length(last_seen_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(last_seen_at, 1, 19) || '.' || substr(substr(last_seen_at, 21, max(length(last_seen_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE last_seen_at END,
    revoked_at = CASE
        WHEN revoked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (revoked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(revoked_at) BETWEEN 22 AND 29
              AND substr(revoked_at, 21, length(revoked_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(revoked_at, 1, 19) || '.' || substr(substr(revoked_at, 21, max(length(revoked_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE revoked_at END
WHERE length(enrolled_at) <> 30 OR length(last_seen_at) <> 30 OR length(revoked_at) <> 30;

UPDATE worker_join_tokens SET
    expires_at = CASE
        WHEN expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(expires_at) BETWEEN 22 AND 29
              AND substr(expires_at, 21, length(expires_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(expires_at, 1, 19) || '.' || substr(substr(expires_at, 21, max(length(expires_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE expires_at END,
    used_at = CASE
        WHEN used_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (used_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(used_at) BETWEEN 22 AND 29
              AND substr(used_at, 21, length(used_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(used_at, 1, 19) || '.' || substr(substr(used_at, 21, max(length(used_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE used_at END,
    created_at = CASE
        WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(created_at) BETWEEN 22 AND 29
              AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(created_at, 1, 19) || '.' || substr(substr(created_at, 21, max(length(created_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE created_at END
WHERE length(expires_at) <> 30 OR length(used_at) <> 30 OR length(created_at) <> 30;

UPDATE workers SET
    last_heartbeat_at = CASE
        WHEN last_heartbeat_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (last_heartbeat_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(last_heartbeat_at) BETWEEN 22 AND 29
              AND substr(last_heartbeat_at, 21, length(last_heartbeat_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(last_heartbeat_at, 1, 19) || '.' || substr(substr(last_heartbeat_at, 21, max(length(last_heartbeat_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE last_heartbeat_at END,
    registered_at = CASE
        WHEN registered_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (registered_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(registered_at) BETWEEN 22 AND 29
              AND substr(registered_at, 21, length(registered_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(registered_at, 1, 19) || '.' || substr(substr(registered_at, 21, max(length(registered_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE registered_at END,
    updated_at = CASE
        WHEN updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
          OR (updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9]*Z'
              AND length(updated_at) BETWEEN 22 AND 29
              AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*')
        THEN substr(updated_at, 1, 19) || '.' || substr(substr(updated_at, 21, max(length(updated_at) - 21, 0)) || '000000000', 1, 9) || 'Z'
        ELSE updated_at END
WHERE length(last_heartbeat_at) <> 30 OR length(registered_at) <> 30 OR length(updated_at) <> 30;

-- +goose Down

-- Nothing to undo: every release parses the nine-digit form, so a downgraded
-- binary reads these rows correctly, and re-trimming them would only bring
-- back the mis-ordering this fixed.
