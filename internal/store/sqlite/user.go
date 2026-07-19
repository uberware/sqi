// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlInsertUser = `
INSERT INTO users (id, username, display_name, password_hash, role, disabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, username, display_name, password_hash, role, disabled, created_at, updated_at`

	sqlGetUser = `
SELECT id, username, display_name, password_hash, role, disabled, created_at, updated_at
FROM users WHERE id = ?`

	sqlGetUserByUsername = `
SELECT id, username, display_name, password_hash, role, disabled, created_at, updated_at
FROM users WHERE username = ? COLLATE NOCASE`

	sqlListUsers = `
SELECT id, username, display_name, password_hash, role, disabled, created_at, updated_at
FROM users ORDER BY username`

	sqlUpdateUser = `
UPDATE users SET display_name = ?, role = ?, disabled = ?, updated_at = ?
WHERE id = ?
RETURNING id, username, display_name, password_hash, role, disabled, created_at, updated_at`

	sqlSetUserPassword = `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?` //nolint:gosec // G101: SQL text, not a credential

	sqlSetUserDisplayName = `
UPDATE users SET display_name = ?, updated_at = ?
WHERE id = ?
RETURNING id, username, display_name, password_hash, role, disabled, created_at, updated_at`
	sqlDeleteUser  = `DELETE FROM users WHERE id = ?`
	sqlCountUsers  = `SELECT COUNT(*) FROM users`
	sqlCountAdmins = `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`
)

func scanUser(row scanner) (store.User, error) {
	var u store.User
	var disabled int
	var createdAt, updatedAt string
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash,
		&u.Role, &disabled, &createdAt, &updatedAt); err != nil {
		return store.User{}, err
	}
	u.Disabled = disabled != 0
	u.CreatedAt = mustTime(createdAt)
	u.UpdatedAt = mustTime(updatedAt)
	return u, nil
}

// CreateUser implements [store.UserStore].
func (s *Store) CreateUser(ctx context.Context, u store.User) (store.User, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertUser.QueryRowContext(ctx, u.ID, u.Username, u.DisplayName,
		u.PasswordHash, u.Role, boolToInt(u.Disabled), now, now)
	out, err := scanUser(row)
	return out, mapErr(err)
}

// GetUser implements [store.UserStore].
func (s *Store) GetUser(ctx context.Context, id string) (store.User, error) {
	out, err := scanUser(s.stmtGetUser.QueryRowContext(ctx, id))
	return out, mapErr(err)
}

// GetUserByUsername implements [store.UserStore].
func (s *Store) GetUserByUsername(ctx context.Context, username string) (store.User, error) {
	out, err := scanUser(s.stmtGetUserByUsername.QueryRowContext(ctx, username))
	return out, mapErr(err)
}

// ListUsers implements [store.UserStore].
func (s *Store) ListUsers(ctx context.Context) ([]store.User, error) {
	rows, err := s.stmtListUsers.QueryContext(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var users []store.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// UpdateUser implements [store.UserStore].
func (s *Store) UpdateUser(ctx context.Context, u store.User) (store.User, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateUser.QueryRowContext(ctx, u.DisplayName, u.Role, boolToInt(u.Disabled), now, u.ID)
	out, err := scanUser(row)
	return out, mapErr(err)
}

// SetUserPassword implements [store.UserStore].
func (s *Store) SetUserPassword(ctx context.Context, id, passwordHash string) error {
	now := timeToText(time.Now().UTC())
	res, err := s.stmtSetUserPassword.ExecContext(ctx, passwordHash, now, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// SetUserPasswordAndEvictSessions implements [store.UserStore]. Both writes
// share one transaction so a caller can report failure honestly.
func (s *Store) SetUserPasswordAndEvictSessions(ctx context.Context, id, passwordHash string) error {
	now := timeToText(time.Now().UTC())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	res, err := tx.ExecContext(ctx, sqlSetUserPassword, passwordHash, now, id)
	if err != nil {
		return mapErr(err)
	}
	if err := checkRowsAffected(res); err != nil {
		return err
	}
	// No rows is fine here: a user with no live sessions is normal.
	if _, err := tx.ExecContext(ctx, sqlDeleteSessionsForUser, id); err != nil {
		return mapErr(err)
	}
	return mapErr(tx.Commit())
}

// SetUserDisplayName implements [store.UserStore].
func (s *Store) SetUserDisplayName(ctx context.Context, id, displayName string) (store.User, error) {
	row := s.stmtSetUserDisplayName.QueryRowContext(ctx, displayName, timeToText(time.Now().UTC()), id)
	out, err := scanUser(row)
	return out, mapErr(err)
}

// DeleteUser implements [store.UserStore].
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.stmtDeleteUser.ExecContext(ctx, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// CountUsers implements [store.UserStore].
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.stmtCountUsers.QueryRowContext(ctx).Scan(&n)
	return n, mapErr(err)
}

// CountAdmins implements [store.UserStore].
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.stmtCountAdmins.QueryRowContext(ctx).Scan(&n)
	return n, mapErr(err)
}
