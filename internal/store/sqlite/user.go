// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	sqlInsertUser = `
INSERT INTO users (id, username, display_name, password_hash, role, auth_source, external_id, disabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, username, display_name, password_hash, role, auth_source, external_id, disabled, created_at, updated_at`

	sqlGetUser = `
SELECT id, username, display_name, password_hash, role, auth_source, external_id, disabled, created_at, updated_at
FROM users WHERE id = ?`

	sqlGetUserByUsername = `
SELECT id, username, display_name, password_hash, role, auth_source, external_id, disabled, created_at, updated_at
FROM users WHERE username = ? COLLATE NOCASE`

	sqlGetUserByExternalID = `
SELECT id, username, display_name, password_hash, role, auth_source, external_id, disabled, created_at, updated_at
FROM users WHERE auth_source = ? AND external_id = ? AND external_id != ''`

	sqlListUsers = `
SELECT id, username, display_name, password_hash, role, auth_source, external_id, disabled, created_at, updated_at
FROM users ORDER BY username`

	// sqlUpdateUserHead is the write that plain and guarded updates share, so a
	// future column lands in both or in neither; its placeholders are bound by
	// updateUserArgs. auth_source and external_id are deliberately absent from
	// the SET list: an account's credential backend and its provider-assigned
	// identity are both fixed at creation.
	sqlUpdateUserHead = `
UPDATE users SET display_name = ?, role = ?, disabled = ?, updated_at = ?
WHERE id = ?`

	// sqlUserReturning hands back the whole updated row, in scanUser's order.
	sqlUserReturning = `
RETURNING id, username, display_name, password_hash, role, auth_source, external_id, disabled, created_at, updated_at`

	sqlUpdateUser = sqlUpdateUserHead + sqlUserReturning

	sqlSetUserPassword = `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?` //nolint:gosec // G101: SQL text, not a credential

	sqlSetUserDisplayName = `
UPDATE users SET display_name = ?, updated_at = ?
WHERE id = ?
RETURNING id, username, display_name, password_hash, role, auth_source, external_id, disabled, created_at, updated_at`
	sqlCountUsers  = `SELECT COUNT(*) FROM users`
	sqlCountAdmins = `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`

	// sqlLastLiveAdmin is true for the row being written when it is an enabled
	// admin and no other enabled admin exists (invariant I4). The count
	// includes the row itself, so "<= 1" means it is the only one. It names the
	// row's own columns unqualified, so it is only valid inside a statement
	// whose target table is users.
	sqlLastLiveAdmin = `(role = 'admin' AND disabled = 0
	AND (SELECT COUNT(*) FROM users o WHERE o.role = 'admin' AND o.disabled = 0) <= 1)`

	// sqlDeleteUserKeepingAdmin deletes the user unless it is the last enabled
	// admin. Zero rows affected means unknown id or refused; the caller tells
	// them apart.
	sqlDeleteUserKeepingAdmin = `DELETE FROM users WHERE id = ? AND NOT ` + sqlLastLiveAdmin

	// sqlUpdateUserKeepingAdmin is sqlUpdateUser refused when the row is the
	// last enabled admin and the new role/disabled pair would make it
	// something else. The two trailing placeholders follow updateUserArgs's and
	// are the NEW role and disabled values, so a harmless edit of the last
	// admin still lands.
	sqlUpdateUserKeepingAdmin = sqlUpdateUserHead + `
  AND NOT (` + sqlLastLiveAdmin + ` AND NOT (? = 'admin' AND ? = 0))` + sqlUserReturning

	sqlListLiveAdminIDs = `SELECT id FROM users WHERE role = 'admin' AND disabled = 0 ORDER BY id`
)

func scanUser(row scanner) (store.User, error) {
	var u store.User
	var disabled int
	var createdAt, updatedAt string
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash,
		&u.Role, &u.AuthSource, &u.ExternalID, &disabled, &createdAt, &updatedAt); err != nil {
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
	if u.AuthSource == "" {
		u.AuthSource = store.AuthSourceLocal
	}
	row := s.stmtInsertUser.QueryRowContext(ctx, u.ID, u.Username, u.DisplayName,
		u.PasswordHash, u.Role, u.AuthSource, u.ExternalID, boolToInt(u.Disabled), now, now)
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

// GetUserByExternalID implements [store.UserStore].
func (s *Store) GetUserByExternalID(ctx context.Context, authSource, externalID string) (store.User, error) {
	out, err := scanUser(s.stmtGetUserByExternalID.QueryRowContext(ctx, authSource, externalID))
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
	row := s.stmtUpdateUser.QueryRowContext(ctx, updateUserArgs(u, timeToText(time.Now().UTC()))...)
	out, err := scanUser(row)
	return out, mapErr(err)
}

// updateUserArgs binds sqlUpdateUserHead's placeholders, in order. Plain and
// guarded updates both take their arguments from here.
func updateUserArgs(u store.User, now string) []any {
	return []any{u.DisplayName, u.Role, boolToInt(u.Disabled), now, u.ID}
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

// UpdateUserKeepingAdmin implements [store.UserStore]. The guard is in the
// UPDATE's WHERE clause, so the refusal and the write are one statement.
func (s *Store) UpdateUserKeepingAdmin(ctx context.Context, u store.User) (store.User, error) {
	var out store.User
	err := s.withAdminAnchors(ctx, func(tx *sql.Tx) error {
		// The guard's own two placeholders carry the NEW role and disabled values.
		args := append(updateUserArgs(u, timeToText(time.Now().UTC())), u.Role, boolToInt(u.Disabled))
		row := tx.QueryRowContext(ctx, sqlUpdateUserKeepingAdmin, args...)
		var err error
		out, err = scanUser(row)
		if errors.Is(err, sql.ErrNoRows) {
			return userMissingOrLastAdmin(ctx, tx, u.ID)
		}
		return mapErr(err)
	})
	return out, err
}

// DeleteUser implements [store.UserStore]. The guard is in the DELETE's WHERE
// clause, so the refusal and the write are one statement.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.withAdminAnchors(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, sqlDeleteUserKeepingAdmin, id)
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
		return userMissingOrLastAdmin(ctx, tx, id)
	})
}

// withAdminAnchors runs fn in a write transaction after taking the anchor
// locks for the last-admin guard (invariant I4).
//
// Order (spec 4.1): every enabled admin's user row, sorted by id, then the
// guarded write inside fn. The guard counts the OTHER enabled admins, so two
// concurrent writes that each demote a different admin would each see the other
// still enabled and both pass; holding the whole admin set serializes them, and
// the fixed id order keeps two such transactions from deadlocking on PostgreSQL.
// On SQLite lockAnchors does nothing and the single write connection provides
// the serialization; the guard in the statement is what makes the answer right.
func (s *Store) withAdminAnchors(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	anchors, err := liveAdminAnchorsTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := lockAnchors(ctx, tx, anchors...); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return mapErr(tx.Commit())
}

// liveAdminAnchorsTx returns the anchors of every enabled admin's user row,
// sorted by id.
func liveAdminAnchorsTx(ctx context.Context, tx *sql.Tx) ([]anchor, error) {
	rows, err := tx.QueryContext(ctx, sqlListLiveAdminIDs)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var anchors []anchor
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		anchors = append(anchors, userAnchor(id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return anchors, nil
}

// userMissingOrLastAdmin explains a guarded write that touched no row: the id
// is unknown ([store.ErrNotFound]) or the guard refused it
// ([store.ErrLastAdmin]).
func userMissingOrLastAdmin(ctx context.Context, tx *sql.Tx, id string) error {
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, id).Scan(&one); err != nil {
		return mapErr(err) // sql.ErrNoRows becomes ErrNotFound
	}
	return store.ErrLastAdmin
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
