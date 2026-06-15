// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── Usage pools ───────────────────────────────────────────────────────────────

const (
	sqlInsertPool = `
INSERT INTO usage_pools (id, name, server_hint, max_concurrent, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id, name, server_hint, max_concurrent, created_at, updated_at`

	sqlGetPool = `
SELECT id, name, server_hint, max_concurrent, created_at, updated_at
FROM usage_pools WHERE id = ?`

	sqlListPools = `
SELECT id, name, server_hint, max_concurrent, created_at, updated_at
FROM usage_pools ORDER BY name`

	// Lists every pool alongside its active-claim count in a single query.
	// The LEFT JOIN keeps pools with zero active claims (COUNT → 0).
	sqlListPoolUsage = `
SELECT p.id, p.name, p.server_hint, p.max_concurrent, p.created_at, p.updated_at,
       COUNT(c.id) AS in_use
FROM   usage_pools p
LEFT JOIN usage_claims c
       ON c.pool_id = p.id AND c.released_at IS NULL
GROUP BY p.id
ORDER BY p.name`

	sqlUpdatePool = `
UPDATE usage_pools
SET name = ?, server_hint = ?, max_concurrent = ?, updated_at = ?
WHERE id = ?
RETURNING id, name, server_hint, max_concurrent, created_at, updated_at`

	sqlDeletePool = `DELETE FROM usage_pools WHERE id = ?`
)

func scanPool(row scanner) (store.UsagePool, error) {
	var p store.UsagePool
	var createdAt, updatedAt string
	if err := row.Scan(
		&p.ID, &p.Name, &p.ServerHint, &p.MaxConcurrent,
		&createdAt, &updatedAt,
	); err != nil {
		return store.UsagePool{}, err
	}
	p.CreatedAt = mustTime(createdAt)
	p.UpdatedAt = mustTime(updatedAt)
	return p, nil
}

// CreateUsagePool implements [store.UsagePoolStore].
func (s *Store) CreateUsagePool(ctx context.Context, pool store.UsagePool) (store.UsagePool, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertPool.QueryRowContext(ctx,
		pool.ID, pool.Name, pool.ServerHint, pool.MaxConcurrent, now, now)
	out, err := scanPool(row)
	return out, mapErr(err)
}

// GetUsagePool implements [store.UsagePoolStore].
func (s *Store) GetUsagePool(ctx context.Context, id string) (store.UsagePool, error) {
	row := s.stmtGetPool.QueryRowContext(ctx, id)
	out, err := scanPool(row)
	return out, mapErr(err)
}

// ListUsagePools implements [store.UsagePoolStore].
func (s *Store) ListUsagePools(ctx context.Context) ([]store.UsagePool, error) {
	rows, err := s.stmtListPools.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var pools []store.UsagePool
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		pools = append(pools, p)
	}
	return pools, rows.Err()
}

// ListUsagePoolUtilization implements [store.UsagePoolStore].
func (s *Store) ListUsagePoolUtilization(ctx context.Context) ([]store.UsagePoolUtilization, error) {
	rows, err := s.stmtListPoolUsage.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var usage []store.UsagePoolUtilization
	for rows.Next() {
		var u store.UsagePoolUtilization
		var createdAt, updatedAt string
		if err := rows.Scan(
			&u.ID, &u.Name, &u.ServerHint, &u.MaxConcurrent,
			&createdAt, &updatedAt, &u.InUse,
		); err != nil {
			return nil, err
		}
		u.CreatedAt = mustTime(createdAt)
		u.UpdatedAt = mustTime(updatedAt)
		usage = append(usage, u)
	}
	return usage, rows.Err()
}

// UpdateUsagePool implements [store.UsagePoolStore].
func (s *Store) UpdateUsagePool(ctx context.Context, pool store.UsagePool) (store.UsagePool, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdatePool.QueryRowContext(ctx,
		pool.Name, pool.ServerHint, pool.MaxConcurrent, now, pool.ID)
	out, err := scanPool(row)
	return out, mapErr(err)
}

// DeleteUsagePool implements [store.UsagePoolStore].
func (s *Store) DeleteUsagePool(ctx context.Context, id string) error {
	res, err := s.stmtDeletePool.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// ── Usage claims ──────────────────────────────────────────────────────────────

const (
	sqlInsertClaim = `
INSERT INTO usage_claims (id, pool_id, task_attempt_id, checked_out_at, released_at)
VALUES (?, ?, ?, ?, NULL)`

	sqlReleaseClaim = `
UPDATE usage_claims SET released_at = ? WHERE id = ?`

	// Counts claims where released_at IS NULL (active claim).
	sqlActiveClaimCount = `
SELECT COUNT(*) FROM usage_claims
WHERE pool_id = ? AND released_at IS NULL`

	// Releases all active claims for a given task attempt.
	sqlReleaseAttemptClaims = `
UPDATE usage_claims
SET released_at = ?
WHERE task_attempt_id = ? AND released_at IS NULL`

	// Releases all active claims held by any attempt belonging to the given
	// job. Used during job cancellation to free all usage pool slots in
	// a single UPDATE rather than iterating through individual attempts.
	sqlReleaseJobClaims = `
UPDATE usage_claims
SET    released_at = ?
WHERE  released_at IS NULL
  AND  task_attempt_id IN (
         SELECT ta.id
         FROM   task_attempts ta
         JOIN   tasks          t  ON ta.task_id = t.id
         WHERE  t.job_id = ?
       )`
)

// CreateClaim implements [store.UsageClaimStore].
func (s *Store) CreateClaim(ctx context.Context, claim store.UsageClaim) (store.UsageClaim, error) {
	now := timeToText(time.Now().UTC())
	_, err := s.stmtInsertClaim.ExecContext(ctx,
		claim.ID, claim.PoolID, claim.TaskAttemptID, now)
	if err != nil {
		return store.UsageClaim{}, mapErr(err)
	}
	claim.ClaimedAt = time.Now().UTC()
	claim.ReleasedAt = nil
	return claim, nil
}

// ReleaseClaim implements [store.UsageClaimStore].
func (s *Store) ReleaseClaim(ctx context.Context, id string, releasedAt time.Time) error {
	res, err := s.stmtReleaseClaim.ExecContext(ctx, timeToText(releasedAt), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// ActiveClaimCount implements [store.UsageClaimStore].
func (s *Store) ActiveClaimCount(ctx context.Context, poolID string) (int, error) {
	var n int
	err := s.stmtActiveClaimCount.QueryRowContext(ctx, poolID).Scan(&n)
	return n, mapErr(err)
}

// TryClaimSlots implements [store.UsageClaimStore].
//
// It opens a transaction, counts active claims for each pool in claims, and
// either inserts all claim rows (all pools have capacity) or rolls back and
// returns [store.ErrUsageAtCapacity] (at least one pool is saturated).
//
// The transaction is serialized by the single-connection pool (SetMaxOpenConns(1))
// so no other goroutine can modify claim counts between the count check and
// the inserts.
func (s *Store) TryClaimSlots(
	ctx context.Context,
	taskAttemptID string,
	claims []store.UsagePoolClaim,
	claimedAt time.Time,
) error {
	if len(claims) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin tx for usage claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is best-effort after commit

	claimedAtText := timeToText(claimedAt)

	for _, c := range claims {
		if c.MaxConcurrent <= 0 {
			// Pool is configured as unlimited; skip count check.
			continue
		}
		var active int
		row := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM usage_claims WHERE pool_id = ? AND released_at IS NULL`,
			c.PoolID)
		if err = row.Scan(&active); err != nil {
			return fmt.Errorf("sqlite: count active claims for pool %q: %w", c.PoolName, mapErr(err))
		}
		if active >= c.MaxConcurrent {
			return store.ErrUsageAtCapacity
		}
	}

	// All pools have capacity — insert the claim rows.
	for _, c := range claims {
		if _, err = tx.ExecContext(
			ctx,
			`INSERT INTO usage_claims (id, pool_id, task_attempt_id, checked_out_at, released_at) VALUES (?, ?, ?, ?, NULL)`,
			c.ClaimID, c.PoolID, taskAttemptID, claimedAtText,
		); err != nil {
			return fmt.Errorf("sqlite: insert claim for pool %q: %w", c.PoolName, mapErr(err))
		}
	}

	return tx.Commit()
}

// ReleaseAttemptClaims implements [store.UsageClaimStore].
func (s *Store) ReleaseAttemptClaims(ctx context.Context, taskAttemptID string, releasedAt time.Time) (int, error) {
	res, err := s.stmtReleaseAttemptClaims.ExecContext(ctx, timeToText(releasedAt), taskAttemptID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ReleaseJobClaims implements [store.UsageClaimStore].
func (s *Store) ReleaseJobClaims(ctx context.Context, jobID string, releasedAt time.Time) (int, error) {
	res, err := s.stmtReleaseJobClaims.ExecContext(ctx, timeToText(releasedAt), jobID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
