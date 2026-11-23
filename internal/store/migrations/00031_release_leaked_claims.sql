-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- Restores invariant I3 ("an active usage claim exists iff its attempt is
-- open") on a database holding claims left active on closed attempts or
-- terminal tasks, by offline-worker reclaim, rejected terminal reports,
-- lease-vs-cancel or the stale-assignment reaper. Each such claim holds a
-- licence slot until its job is deleted. Pure data repair; no schema change. A
-- healthy database is left byte-for-byte as is.
--
-- The predicate is the I3 checker's (sqlClaimInvariantViolations in
-- internal/store/sqlstore/invariants.go): the attempt is missing or not running,
-- or its task is terminal. released_at is written in the layout the Go code
-- parses (RFC 3339 with fractional seconds, UTC), from SQLite's own clock.
UPDATE usage_claims
SET    released_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE  released_at IS NULL
  AND  (NOT EXISTS (SELECT 1 FROM task_attempts a
                    WHERE a.id = usage_claims.task_attempt_id AND a.status = 'running')
        OR EXISTS (SELECT 1 FROM task_attempts a JOIN tasks t ON t.id = a.task_id
                   WHERE a.id = usage_claims.task_attempt_id
                     AND t.status IN ('succeeded', 'failed', 'canceled')));

-- +goose Down

-- A repair is not reversible: which claims were leaked is not recorded, and
-- re-activating them would re-introduce the leak. Down is a documented no-op.
SELECT 1;
