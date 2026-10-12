// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

// sqliteDialect is the SQLite (modernc.org/sqlite) dialect. The values it
// binds match the forms the schema stores: timestamps as 30-character text,
// booleans as INTEGER 0 or 1.
type sqliteDialect struct{}

// Rebind returns query unchanged: SQLite takes "?" placeholders.
func (sqliteDialect) Rebind(query string) string { return query }

// Classify classifies an error by its extended result code (the driver
// enables extended codes on every connection).
func (sqliteDialect) Classify(err error) sqlstore.ErrClass {
	if errors.Is(err, sql.ErrNoRows) {
		return sqlstore.ErrClassNotFound
	}
	if se, ok := errors.AsType[*msqlite.Error](err); ok {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return sqlstore.ErrClassConflict
		}
	}
	return sqlstore.ErrClassOther
}

// LockAnchors does nothing: the store's single write connection already
// serializes every write transaction.
func (sqliteDialect) LockAnchors(context.Context, *sql.Tx, ...sqlstore.Anchor) error { return nil }

// BindTime binds the 30-character text form timestamps are stored in.
func (sqliteDialect) BindTime(t time.Time) any { return sqlstore.TimeText(t) }

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

// ForUpdate returns "": SQLite has no row locks, and its single write
// connection already serializes every write transaction.
func (sqliteDialect) ForUpdate() string { return "" }
