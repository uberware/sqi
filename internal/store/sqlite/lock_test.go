// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/uberware/sqi/internal/fsutil/fsutiltest"
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
	lockPath, err := sqlite.LockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file %s: %v", lockPath, err)
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

// assertSharedLock holds the lock through first and requires every other name
// to be refused with ErrServerLockHeld: two names for one database must not
// give two servers a lock each.
func assertSharedLock(t *testing.T, first string, others ...string) {
	t.Helper()
	l, err := sqlite.AcquireLock(first)
	if err != nil {
		t.Fatalf("AcquireLock(%q): %v", first, err)
	}
	defer l.Release() //nolint:errcheck // test cleanup
	for _, other := range others {
		if l2, err := sqlite.AcquireLock(other); !errors.Is(err, sqlite.ErrServerLockHeld) {
			if l2 != nil {
				_ = l2.Release() //nolint:errcheck // test cleanup
			}
			t.Errorf("AcquireLock(%q) while %q is held = %v, want ErrServerLockHeld", other, first, err)
		}
	}
}

func TestAcquireLock_SymlinkedDatabaseSharesTheLock(t *testing.T) {
	target := filepath.Join(t.TempDir(), "sqi.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.db")
	fsutiltest.SymlinkOrSkip(t, link, target)
	assertSharedLock(t, target, link)
}

// A lock file named through a linked directory is already the same file; this
// guards that resolving the database path keeps it so.
func TestAcquireLock_SymlinkedDirectorySharesTheLock(t *testing.T) {
	targetDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "linkdir")
	fsutiltest.DirLink(t, linkDir, targetDir)
	// No database yet: a server creates it, and the lock is taken first.
	assertSharedLock(t, filepath.Join(targetDir, "sqi.db"), filepath.Join(linkDir, "sqi.db"))
}

// A link whose target does not exist yet is the database SQLite creates
// through it, so it shares the target's lock.
func TestAcquireLock_DanglingSymlinkSharesItsTargetsLock(t *testing.T) {
	target := filepath.Join(t.TempDir(), "sqi.db")
	link := filepath.Join(t.TempDir(), "link.db")
	fsutiltest.SymlinkOrSkip(t, link, target)
	assertSharedLock(t, link, target)
}

// A relative link resolves against its own directory, not the working one.
func TestAcquireLock_RelativeDanglingSymlinkResolvesBesideTheLink(t *testing.T) {
	dir := t.TempDir()
	fsutiltest.SymlinkOrSkip(t, filepath.Join(dir, "link.db"), "sqi.db")
	assertSharedLock(t, filepath.Join(dir, "link.db"), filepath.Join(dir, "sqi.db"))
}

// An in-memory database has no file to resolve, so its name is kept.
func TestResolvePath_KeepsAnInMemoryDatabase(t *testing.T) {
	for _, name := range []string{":memory:", "file::memory:", "file:x?mode=memory&cache=shared"} {
		if got, err := sqlite.ResolvePath(name); err != nil || got != name {
			t.Errorf("ResolvePath(%q) = %q, %v; want it unchanged", name, got, err)
		}
	}
}
