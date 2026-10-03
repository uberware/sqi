// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"fmt"
)

// sqlClaimInvariantViolations lists active claims that break invariant I3
// ("an active usage claim exists iff its attempt is open"): the attempt is
// missing or not running, or its task is terminal.
const sqlClaimInvariantViolations = `
SELECT c.id
FROM   usage_claims c
LEFT JOIN task_attempts a ON a.id = c.task_attempt_id
LEFT JOIN tasks t          ON t.id = a.task_id
WHERE  c.released_at IS NULL
  AND  (a.id IS NULL OR a.status != 'running'
        OR t.status IN ('succeeded', 'failed', 'canceled'))
ORDER BY c.id`

// ClaimInvariantViolations returns the IDs of active usage claims that break
// invariant I3. It is a diagnostic used by tests and is not part of
// store.Store. An empty result means the invariant holds.
func (s *Store) ClaimInvariantViolations(ctx context.Context) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, sqlClaimInvariantViolations)
	if err != nil {
		return nil, fmt.Errorf("sqlite: claim invariant violations: %w", mapErr(err))
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: claim invariant violations: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: claim invariant violations: %w", mapErr(err))
	}
	return ids, nil
}
