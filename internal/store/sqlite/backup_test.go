// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/fsutil"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// TestBackup_CopyIsRestricted pins that a backup, which holds password and
// API-key hashes and session tokens, is readable by its writer only, and is a
// working database.
func TestBackup_CopyIsRestricted(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateFarm(t.Context(), store.Farm{ID: "farm-backup", Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "sqi-backup.db")
	if err := s.Backup(t.Context(), out); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if restricted, err := fsutil.IsRestricted(out); err != nil || !restricted {
		t.Fatalf("IsRestricted = %v, %v; want a restricted copy", restricted, err)
	}

	copied, err := sqlite.Open(context.Background(), out, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer copied.Close()
	if f, err := copied.GetFarm(t.Context(), "farm-backup"); err != nil || f.Name != "kept" {
		t.Fatalf("GetFarm from the copy = %+v, %v", f, err)
	}
}

// TestBackup_RefusesAnExistingDestination pins that a backup never replaces a
// file, an empty one included, and leaves it as it was.
func TestBackup_RefusesAnExistingDestination(t *testing.T) {
	s := openTestStore(t)
	out := filepath.Join(t.TempDir(), "sqi-backup.db")
	if err := os.WriteFile(out, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(t.Context(), out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Backup over an existing file = %v; want an already-exists error", err)
	}
	if info, err := os.Stat(out); err != nil || info.Size() != 0 {
		t.Fatalf("existing file after a refused backup: %v, %v", info, err)
	}
}
