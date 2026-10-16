// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"errors"
	"os"
	"testing"

	"github.com/uberware/sqi/internal/store/sqlite"
)

func TestOpen_ServerLockRefusesASecondServer(t *testing.T) {
	path := t.TempDir() + "/test.db"
	first, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: true, ServerLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: true, ServerLock: true}); !errors.Is(err, sqlite.ErrServerLockHeld) {
		t.Fatalf("second Open = %v, want ErrServerLockHeld", err)
	}
	// A CLI that does not ask for the lock still opens (backup, worker tokens).
	cli, err := sqlite.Open(t.Context(), path, sqlite.Options{})
	if err != nil {
		t.Fatalf("Open without ServerLock while a server holds it: %v", err)
	}
	_ = cli.Close()

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := sqlite.Open(t.Context(), path, sqlite.Options{ServerLock: true})
	if err != nil {
		t.Fatalf("Open after Close released the lock: %v", err)
	}
	_ = again.Close()
	if _, err := os.Stat(sqlite.LockPath(path)); err != nil {
		t.Fatalf("lock file %s: %v", sqlite.LockPath(path), err)
	}
}

func TestOpen_ServerLockIsSkippedForAnInMemoryDatabase(t *testing.T) {
	st, err := sqlite.Open(t.Context(), ":memory:", sqlite.Options{AutoMigrate: true, ServerLock: true})
	if err != nil {
		t.Fatalf("in-memory Open with ServerLock: %v", err)
	}
	_ = st.Close()
}

func TestOpen_ServerLockIsReleasedWhenOpenFails(t *testing.T) {
	// A migration failure after the lock is taken must not strand the lock.
	path := t.TempDir() + "/test.db"
	if err := os.WriteFile(path, []byte("this is not a sqlite database, just text padding it out to be long enough"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: true, ServerLock: true}); err == nil {
		t.Fatal("Open of a garbage file succeeded")
	}
	l, err := sqlite.AcquireLock(path)
	if err != nil {
		t.Fatalf("lock still held after a failed Open: %v", err)
	}
	_ = l.Release() //nolint:errcheck // test cleanup
}

func TestStoreClose_IsIdempotentWithServerLock(t *testing.T) {
	path := t.TempDir() + "/test.db"
	st, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: true, ServerLock: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}
