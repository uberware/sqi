// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// sqliteDialect is the SQLite (modernc.org/sqlite) dialect. Every method
// produces exactly the SQL text or value this store has always sent SQLite.
type sqliteDialect struct{}

// Rebind returns query unchanged: SQLite takes "?" placeholders.
func (sqliteDialect) Rebind(query string) string { return query }

// Classify classifies an error by its extended result code (the driver
// enables extended codes on every connection).
func (sqliteDialect) Classify(err error) ErrClass {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrClassNotFound
	}
	if se, ok := errors.AsType[*msqlite.Error](err); ok {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return ErrClassConflict
		}
	}
	return ErrClassOther
}

// LockAnchors does nothing: the store's single write connection already
// serializes every write transaction.
func (sqliteDialect) LockAnchors(context.Context, *sql.Tx, ...Anchor) error { return nil }

// BindTime binds the 30-character text form timestamps are stored in.
func (sqliteDialect) BindTime(t time.Time) any { return timeToText(t) }

// BindBool binds 1 or 0 (boolean columns are INTEGER).
func (sqliteDialect) BindBool(b bool) any {
	if b {
		return int64(1)
	}
	return int64(0)
}

// CIEqual compares with SQLite's NOCASE collation (ASCII case-folding).
func (sqliteDialect) CIEqual(col string) string { return col + " = ? COLLATE NOCASE" }

// CILike uses LIKE, which SQLite matches ASCII case-insensitively.
func (sqliteDialect) CILike(col string) string { return col + " LIKE ?" }
