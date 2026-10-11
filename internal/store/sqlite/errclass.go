// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"database/sql"
	"errors"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrClass is what a driver error means to the store.
type ErrClass int

// The error classes. ErrClassRetry marks a transaction the database aborted
// for a concurrent writer; it is safe to run again.
const (
	ErrClassOther ErrClass = iota
	ErrClassNotFound
	ErrClassConflict
	ErrClassRetry
)

// classifySQLite classifies an error from modernc.org/sqlite by its extended
// result code (the driver enables extended codes on every connection).
func classifySQLite(err error) ErrClass {
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
