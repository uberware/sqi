// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
		// An ON DELETE RESTRICT foreign key reports as a trigger constraint
		// rather than a foreign-key one; the message tells them apart.
		if se.Code() == sqlite3.SQLITE_CONSTRAINT_TRIGGER && strings.Contains(se.Error(), "FOREIGN KEY constraint failed") {
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

// ForShare returns "": SQLite has no row locks.
func (sqliteDialect) ForShare() string { return "" }

// ClockMicros is the Julian day number of now, rebased to the Unix epoch and
// scaled to microseconds. julianday('now') is one value per statement.
func (sqliteDialect) ClockMicros() string {
	return "CAST((julianday('now') - 2440587.5) * 86400000000 AS INTEGER)"
}

// EpochSeconds is the Julian day number of expr rebased to the Unix epoch and
// scaled to seconds. julianday reads the stored text, fractional digits and
// trailing Z included.
func (sqliteDialect) EpochSeconds(expr string) string {
	return "((julianday(" + expr + ") - 2440587.5) * 86400.0)"
}
