// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
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
