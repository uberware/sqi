// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// shortName is the 8.3 short form of path, which must exist. It skips the test
// where the volume does not generate short names.
func shortName(t *testing.T, path string) string {
	t.Helper()
	long, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatalf("GetShortPathName(%s): %v", path, err)
	}
	short := windows.UTF16ToString(buf[:n])
	if short == path {
		t.Skipf("no 8.3 short name for %s on this volume", path)
	}
	return short
}

// The 8.3 short and the long name of one database file are two names for it,
// so they take one lock.
func TestAcquireLock_ShortAndLongNamesShareTheLock(t *testing.T) {
	db := filepath.Join(t.TempDir(), "a long database name.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertSharedLock(t, db, shortName(t, db))
}
