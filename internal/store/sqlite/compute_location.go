// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlInsertComputeLoc = `
INSERT INTO compute_locations (id, name, description, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
RETURNING id, name, description, created_at, updated_at`

	sqlGetComputeLoc = `
SELECT id, name, description, created_at, updated_at
FROM compute_locations WHERE id = ?`

	sqlGetComputeLocByName = `
SELECT id, name, description, created_at, updated_at
FROM compute_locations WHERE name = ?`

	sqlListComputeLocs = `
SELECT id, name, description, created_at, updated_at
FROM compute_locations ORDER BY name`

	sqlUpdateComputeLoc = `
UPDATE compute_locations
SET name = ?, description = ?, updated_at = ?
WHERE id = ?
RETURNING id, name, description, created_at, updated_at`

	sqlDeleteComputeLoc = `DELETE FROM compute_locations WHERE id = ?`
)

func scanComputeLocation(row scanner) (store.ComputeLocation, error) {
	var loc store.ComputeLocation
	var createdAt, updatedAt string
	if err := row.Scan(&loc.ID, &loc.Name, &loc.Description, &createdAt, &updatedAt); err != nil {
		return store.ComputeLocation{}, err
	}
	loc.CreatedAt = mustTime(createdAt)
	loc.UpdatedAt = mustTime(updatedAt)
	return loc, nil
}

// CreateComputeLocation implements [store.ComputeLocationStore].
func (s *Store) CreateComputeLocation(ctx context.Context, loc store.ComputeLocation) (store.ComputeLocation, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertComputeLoc.QueryRowContext(ctx, loc.ID, loc.Name, loc.Description, now, now)
	out, err := scanComputeLocation(row)
	return out, mapErr(err)
}

// GetComputeLocation implements [store.ComputeLocationStore].
func (s *Store) GetComputeLocation(ctx context.Context, id string) (store.ComputeLocation, error) {
	row := s.stmtGetComputeLoc.QueryRowContext(ctx, id)
	out, err := scanComputeLocation(row)
	return out, mapErr(err)
}

// GetComputeLocationByName implements [store.ComputeLocationStore].
func (s *Store) GetComputeLocationByName(ctx context.Context, name string) (store.ComputeLocation, error) {
	row := s.stmtGetComputeLocByName.QueryRowContext(ctx, name)
	out, err := scanComputeLocation(row)
	return out, mapErr(err)
}

// ListComputeLocations implements [store.ComputeLocationStore].
func (s *Store) ListComputeLocations(ctx context.Context) ([]store.ComputeLocation, error) {
	rows, err := s.stmtListComputeLocs.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var locs []store.ComputeLocation
	for rows.Next() {
		loc, err := scanComputeLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	return locs, rows.Err()
}

// UpdateComputeLocation implements [store.ComputeLocationStore].
func (s *Store) UpdateComputeLocation(ctx context.Context, loc store.ComputeLocation) (store.ComputeLocation, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateComputeLoc.QueryRowContext(ctx, loc.Name, loc.Description, now, loc.ID)
	out, err := scanComputeLocation(row)
	return out, mapErr(err)
}

// DeleteComputeLocation implements [store.ComputeLocationStore].
func (s *Store) DeleteComputeLocation(ctx context.Context, id string) error {
	res, err := s.stmtDeleteComputeLoc.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}
