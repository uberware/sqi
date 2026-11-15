// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dbfault holds the faults a chaos run applies to the store from
// outside the server.
package dbfault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/uberware/sqi/internal/store/sqlite"
)

// lockBusyTimeoutMS is how long the lock holder waits for a write the server
// has in flight to finish before it gets the lock.
const lockBusyTimeoutMS = 60000

// SQLiteLock is an outside connection holding a write lock on a SQLite
// database, as a backup tool or an operator's sqlite3 shell does.
type SQLiteLock struct {
	mu   sync.Mutex
	db   *sql.DB
	conn *sql.Conn
}

// LockSQLite opens path through [sqlite.DSN] with its own busy_timeout of 60 s
// and runs BEGIN IMMEDIATE, returning once the lock is held. While it is held
// every writer of the database waits, or fails with SQLITE_BUSY once its own
// busy_timeout passes.
func LockSQLite(ctx context.Context, path string) (*SQLiteLock, error) {
	dsn, err := sqlite.DSN(path)
	if err != nil {
		return nil, fmt.Errorf("dbfault: SQLite path %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbfault: open %s: %w", path, err)
	}
	// One connection: the pragma and the transaction must share it.
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("dbfault: connect to %s: %w", path, err), db.Close())
	}
	l := &SQLiteLock{db: db, conn: conn}
	for _, stmt := range []string{fmt.Sprintf("PRAGMA busy_timeout=%d", lockBusyTimeoutMS), "BEGIN IMMEDIATE"} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return nil, errors.Join(fmt.Errorf("dbfault: %s on %s: %w", stmt, path, err), l.Release())
		}
	}
	return l, nil
}

// Release rolls back and closes. Safe to call more than once.
func (l *SQLiteLock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil
	}
	conn, db := l.conn, l.db
	l.conn, l.db = nil, nil
	// A background context: Release must work after the context that took the
	// lock is done. ROLLBACK fails harmlessly when no transaction is open.
	_, rerr := conn.ExecContext(context.Background(), "ROLLBACK")
	if rerr != nil {
		rerr = fmt.Errorf("dbfault: roll back the lock: %w", rerr)
	}
	return errors.Join(rerr, conn.Close(), db.Close())
}
