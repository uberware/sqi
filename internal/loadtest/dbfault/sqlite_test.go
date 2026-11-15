// SPDX-License-Identifier: AGPL-3.0-or-later

package dbfault

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/sqlite"
)

// openDB opens path as the server does, WAL included, with its own
// busy_timeout in ms.
func openDB(t *testing.T, path string, busyMS int) *sql.Conn {
	t.Helper()
	dsn, err := sqlite.DSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=" + strconv.Itoa(busyMS)} {
		if _, err := conn.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return conn
}

func TestLockSQLite_BlocksAnotherWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqi.db")
	other := openDB(t, path, 200)
	if _, err := other.ExecContext(t.Context(), "CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}

	lock, err := LockSQLite(t.Context(), path)
	if err != nil {
		t.Fatalf("LockSQLite: %v", err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			lock.Release() //nolint:errcheck // cleanup
		}
	})

	start := time.Now()
	_, err = other.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	if err == nil {
		t.Fatal("BEGIN IMMEDIATE succeeded while the outside lock was held")
	}
	if msg := err.Error(); !strings.Contains(msg, "SQLITE_BUSY") && !strings.Contains(msg, "database is locked") {
		t.Errorf("BEGIN IMMEDIATE err = %v, want SQLITE_BUSY", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the other writer waited %s; its busy_timeout is 200 ms", d)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	released = true
	if _, err := other.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE after Release: %v", err)
	}
	if _, err := other.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Errorf("second Release = %v, want nil", err)
	}
}

func TestLockSQLite_HonorsContextWhileWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqi.db")
	first, err := LockSQLite(t.Context(), path)
	if err != nil {
		t.Fatalf("LockSQLite: %v", err)
	}
	defer first.Release() //nolint:errcheck // cleanup

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	second, err := LockSQLite(ctx, path)
	if err == nil {
		second.Release() //nolint:errcheck // cleanup
		t.Fatal("a second LockSQLite succeeded while the first was held")
	}
}
