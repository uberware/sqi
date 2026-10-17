// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// DSN changes only a path the driver would misread: one holding a "?", which
// the driver takes as the start of its query.
func TestDSN(t *testing.T) {
	for _, path := range []string{
		"", ":memory:", "file::memory:", "file:x?mode=memory&cache=shared",
		"file:/var/lib/sqi/sqi.db?_pragma=busy_timeout(5000)",
		filepath.Join(t.TempDir(), "sqi.db"), filepath.Join(t.TempDir(), "sqi #1 100%.db"),
	} {
		if got, err := sqlite.DSN(path); err != nil || got != path {
			t.Errorf("DSN(%q) = %q, %v; want it unchanged", path, got, err)
		}
	}
	path := filepath.Join(t.TempDir(), "sqi?mode=ro.db")
	got, err := sqlite.DSN(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "file:") || !strings.Contains(got, "sqi%3Fmode=ro.db") {
		t.Errorf("DSN(%q) = %q; want a file: URI with the \"?\" escaped", path, got)
	}
}

// A store opened at a path holding a "?" must open, and create, the file that
// path names, through both its pools: handed to the driver raw, the "?" would
// start a query and the store would open a database named for what precedes
// it.
func TestOpen_APathWithAQuestionMark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`"?" cannot appear in a Windows file name`)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sqi?mode=ro.db")
	s, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: true, ServerLock: true})
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	if _, err := s.CreateFarm(t.Context(), store.Farm{ID: "f1", Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	if f, err := s.GetFarm(t.Context(), "f1"); err != nil || f.Name != "kept" { // the read pool
		t.Fatalf("GetFarm = %+v, %v", f, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no database at %s: %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sqi")); !os.IsNotExist(err) {
		t.Fatalf("a database named for the text before the \"?\" exists (err %v)", err)
	}
}
