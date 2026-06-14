// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── License pools ─────────────────────────────────────────────────────────────

const (
	sqlInsertPool = `
INSERT INTO license_pools (id, name, product, server_hint, max_concurrent, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id, name, product, server_hint, max_concurrent, created_at, updated_at`

	sqlGetPool = `
SELECT id, name, product, server_hint, max_concurrent, created_at, updated_at
FROM license_pools WHERE id = ?`

	sqlListPools = `
SELECT id, name, product, server_hint, max_concurrent, created_at, updated_at
FROM license_pools ORDER BY name`

	sqlUpdatePool = `
UPDATE license_pools
SET name = ?, product = ?, server_hint = ?, max_concurrent = ?, updated_at = ?
WHERE id = ?
RETURNING id, name, product, server_hint, max_concurrent, created_at, updated_at`

	sqlDeletePool = `DELETE FROM license_pools WHERE id = ?`
)

func scanPool(row scanner) (store.LicensePool, error) {
	var p store.LicensePool
	var createdAt, updatedAt string
	if err := row.Scan(
		&p.ID, &p.Name, &p.Product, &p.ServerHint, &p.MaxConcurrent,
		&createdAt, &updatedAt,
	); err != nil {
		return store.LicensePool{}, err
	}
	p.CreatedAt = mustTime(createdAt)
	p.UpdatedAt = mustTime(updatedAt)
	return p, nil
}

// CreateLicensePool implements [store.LicensePoolStore].
func (s *Store) CreateLicensePool(ctx context.Context, pool store.LicensePool) (store.LicensePool, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertPool.QueryRowContext(ctx,
		pool.ID, pool.Name, pool.Product, pool.ServerHint, pool.MaxConcurrent, now, now)
	out, err := scanPool(row)
	return out, mapErr(err)
}

// GetLicensePool implements [store.LicensePoolStore].
func (s *Store) GetLicensePool(ctx context.Context, id string) (store.LicensePool, error) {
	row := s.stmtGetPool.QueryRowContext(ctx, id)
	out, err := scanPool(row)
	return out, mapErr(err)
}

// ListLicensePools implements [store.LicensePoolStore].
func (s *Store) ListLicensePools(ctx context.Context) ([]store.LicensePool, error) {
	rows, err := s.stmtListPools.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var pools []store.LicensePool
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		pools = append(pools, p)
	}
	return pools, rows.Err()
}

// UpdateLicensePool implements [store.LicensePoolStore].
func (s *Store) UpdateLicensePool(ctx context.Context, pool store.LicensePool) (store.LicensePool, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdatePool.QueryRowContext(ctx,
		pool.Name, pool.Product, pool.ServerHint, pool.MaxConcurrent, now, pool.ID)
	out, err := scanPool(row)
	return out, mapErr(err)
}

// DeleteLicensePool implements [store.LicensePoolStore].
func (s *Store) DeleteLicensePool(ctx context.Context, id string) error {
	res, err := s.stmtDeletePool.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// ── License checkouts ─────────────────────────────────────────────────────────

const (
	sqlInsertCheckout = `
INSERT INTO license_checkouts (id, pool_id, task_attempt_id, checked_out_at, released_at)
VALUES (?, ?, ?, ?, NULL)`

	sqlReleaseCheckout = `
UPDATE license_checkouts SET released_at = ? WHERE id = ?`

	// Counts checkouts where released_at IS NULL (active checkout).
	sqlActiveCheckoutCount = `
SELECT COUNT(*) FROM license_checkouts
WHERE pool_id = ? AND released_at IS NULL`

	// Releases all active checkouts for a given task attempt.
	sqlReleaseAttemptCheckouts = `
UPDATE license_checkouts
SET released_at = ?
WHERE task_attempt_id = ? AND released_at IS NULL`

	// Releases all active checkouts held by any attempt belonging to the given
	// job. Used during job cancellation to free all license slots in
	// a single UPDATE rather than iterating through individual attempts.
	sqlReleaseJobCheckouts = `
UPDATE license_checkouts
SET    released_at = ?
WHERE  released_at IS NULL
  AND  task_attempt_id IN (
         SELECT ta.id
         FROM   task_attempts ta
         JOIN   tasks          t  ON ta.task_id = t.id
         WHERE  t.job_id = ?
       )`
)

// CreateCheckout implements [store.LicenseCheckoutStore].
func (s *Store) CreateCheckout(ctx context.Context, checkout store.LicenseCheckout) (store.LicenseCheckout, error) {
	now := timeToText(time.Now().UTC())
	_, err := s.stmtInsertCheckout.ExecContext(ctx,
		checkout.ID, checkout.PoolID, checkout.TaskAttemptID, now)
	if err != nil {
		return store.LicenseCheckout{}, mapErr(err)
	}
	checkout.CheckedOutAt = time.Now().UTC()
	checkout.ReleasedAt = nil
	return checkout, nil
}

// ReleaseCheckout implements [store.LicenseCheckoutStore].
func (s *Store) ReleaseCheckout(ctx context.Context, id string, releasedAt time.Time) error {
	res, err := s.stmtReleaseCheckout.ExecContext(ctx, timeToText(releasedAt), id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// ActiveCheckoutCount implements [store.LicenseCheckoutStore].
func (s *Store) ActiveCheckoutCount(ctx context.Context, poolID string) (int, error) {
	var n int
	err := s.stmtActiveCheckoutCount.QueryRowContext(ctx, poolID).Scan(&n)
	return n, mapErr(err)
}

// TryClaimLicenseSlots implements [store.LicenseCheckoutStore].
//
// It opens a transaction, counts active checkouts for each pool in claims, and
// either inserts all checkout rows (all pools have capacity) or rolls back and
// returns [store.ErrLicenseAtCapacity] (at least one pool is saturated).
//
// The transaction is serialized by the single-connection pool (SetMaxOpenConns(1))
// so no other goroutine can modify checkout counts between the count check and
// the inserts.
func (s *Store) TryClaimLicenseSlots(
	ctx context.Context,
	taskAttemptID string,
	claims []store.LicensePoolClaim,
	checkedOutAt time.Time,
) error {
	if len(claims) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin tx for license claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is best-effort after commit

	checkedOutAtText := timeToText(checkedOutAt)

	for _, c := range claims {
		if c.MaxConcurrent <= 0 {
			// Pool is configured as unlimited; skip count check.
			continue
		}
		var active int
		row := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM license_checkouts WHERE pool_id = ? AND released_at IS NULL`,
			c.PoolID)
		if err = row.Scan(&active); err != nil {
			return fmt.Errorf("sqlite: count active checkouts for pool %q: %w", c.PoolName, mapErr(err))
		}
		if active >= c.MaxConcurrent {
			return store.ErrLicenseAtCapacity
		}
	}

	// All pools have capacity — insert the checkout rows.
	for _, c := range claims {
		if _, err = tx.ExecContext(
			ctx,
			`INSERT INTO license_checkouts (id, pool_id, task_attempt_id, checked_out_at, released_at) VALUES (?, ?, ?, ?, NULL)`,
			c.CheckoutID, c.PoolID, taskAttemptID, checkedOutAtText,
		); err != nil {
			return fmt.Errorf("sqlite: insert checkout for pool %q: %w", c.PoolName, mapErr(err))
		}
	}

	return tx.Commit()
}

// ReleaseAttemptCheckouts implements [store.LicenseCheckoutStore].
func (s *Store) ReleaseAttemptCheckouts(ctx context.Context, taskAttemptID string, releasedAt time.Time) (int, error) {
	res, err := s.stmtReleaseAttemptCheckouts.ExecContext(ctx, timeToText(releasedAt), taskAttemptID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ReleaseJobCheckouts implements [store.LicenseCheckoutStore].
func (s *Store) ReleaseJobCheckouts(ctx context.Context, jobID string, releasedAt time.Time) (int, error) {
	res, err := s.stmtReleaseJobCheckouts.ExecContext(ctx, timeToText(releasedAt), jobID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
