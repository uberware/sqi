// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlInsertStorageLoc = `
INSERT INTO storage_locations (id, name, type, description, roots, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id, name, type, description, roots, created_at, updated_at`

	sqlGetStorageLoc = `
SELECT id, name, type, description, roots, created_at, updated_at
FROM storage_locations WHERE id = ?`

	sqlGetStorageLocByName = `
SELECT id, name, type, description, roots, created_at, updated_at
FROM storage_locations WHERE name = ?`

	sqlListStorageLocs = `
SELECT id, name, type, description, roots, created_at, updated_at
FROM storage_locations ORDER BY name`

	sqlUpdateStorageLoc = `
UPDATE storage_locations
SET name = ?, type = ?, description = ?, roots = ?, updated_at = ?
WHERE id = ?
RETURNING id, name, type, description, roots, created_at, updated_at`

	sqlDeleteStorageLoc = `DELETE FROM storage_locations WHERE id = ?`
)

func scanStorageLocation(row scanner) (store.StorageLocation, error) {
	var loc store.StorageLocation
	var locType, rootsJSON, createdAt, updatedAt string
	if err := row.Scan(
		&loc.ID, &loc.Name, &locType, &loc.Description,
		&rootsJSON, &createdAt, &updatedAt,
	); err != nil {
		return store.StorageLocation{}, err
	}
	loc.Type = store.StorageLocationType(locType)
	roots, err := unmarshalJSON(rootsJSON, map[string]string{})
	if err != nil {
		return store.StorageLocation{}, err
	}
	loc.Roots = roots
	loc.CreatedAt = mustTime(createdAt)
	loc.UpdatedAt = mustTime(updatedAt)
	return loc, nil
}

// CreateStorageLocation implements [store.StorageLocationStore].
func (s *Store) CreateStorageLocation(ctx context.Context, loc store.StorageLocation) (store.StorageLocation, error) {
	rootsJSON, err := marshalJSON(loc.Roots)
	if err != nil {
		return store.StorageLocation{}, err
	}
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertStorageLoc.QueryRowContext(ctx,
		loc.ID, loc.Name, string(loc.Type), loc.Description, rootsJSON, now, now)
	out, err := scanStorageLocation(row)
	return out, mapErr(err)
}

// GetStorageLocation implements [store.StorageLocationStore].
func (s *Store) GetStorageLocation(ctx context.Context, id string) (store.StorageLocation, error) {
	row := s.stmtGetStorageLoc.QueryRowContext(ctx, id)
	out, err := scanStorageLocation(row)
	return out, mapErr(err)
}

// GetStorageLocationByName implements [store.StorageLocationStore].
func (s *Store) GetStorageLocationByName(ctx context.Context, name string) (store.StorageLocation, error) {
	row := s.stmtGetStorageLocByName.QueryRowContext(ctx, name)
	out, err := scanStorageLocation(row)
	return out, mapErr(err)
}

// ListStorageLocations implements [store.StorageLocationStore].
func (s *Store) ListStorageLocations(ctx context.Context) ([]store.StorageLocation, error) {
	rows, err := s.stmtListStorageLocs.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var locs []store.StorageLocation
	for rows.Next() {
		loc, err := scanStorageLocation(rows)
		if err != nil {
			return nil, err
		}
		locs = append(locs, loc)
	}
	return locs, rows.Err()
}

// UpdateStorageLocation implements [store.StorageLocationStore].
func (s *Store) UpdateStorageLocation(ctx context.Context, loc store.StorageLocation) (store.StorageLocation, error) {
	rootsJSON, err := marshalJSON(loc.Roots)
	if err != nil {
		return store.StorageLocation{}, err
	}
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateStorageLoc.QueryRowContext(ctx,
		loc.Name, string(loc.Type), loc.Description, rootsJSON, now, loc.ID)
	out, err := scanStorageLocation(row)
	return out, mapErr(err)
}

// DeleteStorageLocation implements [store.StorageLocationStore].
func (s *Store) DeleteStorageLocation(ctx context.Context, id string) error {
	res, err := s.stmtDeleteStorageLoc.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}
