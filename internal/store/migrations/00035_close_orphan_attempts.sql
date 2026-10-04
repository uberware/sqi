-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up

-- Closes attempts earlier releases left open (H4a2 §5.5, tracker item 9 vii):
-- a running attempt whose task is not in flight (F4's orphan on a finished
-- task, or a task back in ready/pending), and a running attempt that is not its
-- task's latest (v0.3.0's F5 residue: the old reaper closed the newer attempt
-- and left the older one open). 00031 released claims only and could not reach
-- these. Each is closed as failed with server time, then every active claim
-- whose attempt is no longer running is released (00031's predicate). A
-- healthy database is unchanged.
UPDATE task_attempts
SET    status   = 'failed',
       ended_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
       message  = CASE WHEN COALESCE(message, '') = '' THEN 'closed by repair: orphaned attempt' ELSE message END
WHERE  status = 'running'
  AND  (EXISTS (SELECT 1 FROM tasks t WHERE t.id = task_attempts.task_id
                AND t.status NOT IN ('assigned', 'running'))
        OR attempt_number < (SELECT MAX(a2.attempt_number) FROM task_attempts a2
                             WHERE a2.task_id = task_attempts.task_id));

UPDATE usage_claims
SET    released_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE  released_at IS NULL
  AND  NOT EXISTS (SELECT 1 FROM task_attempts a
                   WHERE a.id = usage_claims.task_attempt_id AND a.status = 'running');

-- +goose Down

-- A repair is not reversible. Down is a documented no-op.
SELECT 1;
