// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlInsertFarm = `
INSERT INTO farms (id, name, description, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id, name, description, created_at, updated_at`

	sqlGetFarm = `
SELECT id, name, description, created_at, updated_at
FROM farms WHERE id = ?`

	sqlListFarms = `
SELECT id, name, description, created_at, updated_at
FROM farms ORDER BY name`

	sqlUpdateFarm = `
UPDATE farms
SET name = ?, description = ?, updated_at = ?
WHERE id = ?
RETURNING id, name, description, created_at, updated_at`

	sqlDeleteFarm = `DELETE FROM farms WHERE id = ?`
)

func scanFarm(row scanner) (store.Farm, error) {
	var f store.Farm
	var createdAt, updatedAt string
	if err := row.Scan(&f.ID, &f.Name, &f.Description, &createdAt, &updatedAt); err != nil {
		return store.Farm{}, err
	}
	f.CreatedAt = mustTime(createdAt)
	f.UpdatedAt = mustTime(updatedAt)
	return f, nil
}

// CreateFarm implements [store.FarmStore].
func (s *Store) CreateFarm(ctx context.Context, farm store.Farm) (store.Farm, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertFarm.QueryRowContext(ctx,
		farm.ID, farm.Name, farm.Description, now, now)
	out, err := scanFarm(row)
	return out, mapErr(err)
}

// GetFarm implements [store.FarmStore].
func (s *Store) GetFarm(ctx context.Context, id string) (store.Farm, error) {
	row := s.stmtGetFarm.QueryRowContext(ctx, id)
	out, err := scanFarm(row)
	return out, mapErr(err)
}

// ListFarms implements [store.FarmStore].
func (s *Store) ListFarms(ctx context.Context) ([]store.Farm, error) {
	rows, err := s.stmtListFarms.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var farms []store.Farm
	for rows.Next() {
		f, err := scanFarm(rows)
		if err != nil {
			return nil, err
		}
		farms = append(farms, f)
	}
	return farms, rows.Err()
}

// UpdateFarm implements [store.FarmStore].
func (s *Store) UpdateFarm(ctx context.Context, farm store.Farm) (store.Farm, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateFarm.QueryRowContext(ctx,
		farm.Name, farm.Description, now, farm.ID)
	out, err := scanFarm(row)
	return out, mapErr(err)
}

// DeleteFarm implements [store.FarmStore].
func (s *Store) DeleteFarm(ctx context.Context, id string) error {
	res, err := s.stmtDeleteFarm.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}
