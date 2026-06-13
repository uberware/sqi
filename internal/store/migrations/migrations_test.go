// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/store/migrations"
)

func TestFS_ListsRealMigrationsOnly(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one embedded .sql migration")
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "._") {
			t.Errorf("AppleDouble file leaked into listing: %s", e.Name())
		}
		if !strings.HasSuffix(e.Name(), ".sql") {
			t.Errorf("unexpected non-sql entry: %s", e.Name())
		}
	}
}

func TestFS_OpenAppleDoubleIsHidden(t *testing.T) {
	// Opening a ._-prefixed name must report not-exist even though the raw
	// embed might contain it on an APFS checkout.
	_, err := migrations.FS.Open("._0001_init.sql")
	if err == nil {
		t.Fatal("Open of AppleDouble file: want error, got nil")
	}
	var perr *fs.PathError
	if !errors.As(err, &perr) {
		t.Fatalf("want *fs.PathError, got %T", err)
	}
}

func TestFS_OpenRealMigrationSucceeds(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	f, err := migrations.FS.Open(entries[0].Name())
	if err != nil {
		t.Fatalf("Open(%s): %v", entries[0].Name(), err)
	}
	_ = f.Close()
}
