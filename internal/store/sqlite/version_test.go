// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

// bumpVersion migrates a SQLite file and records one more applied version than
// this binary knows, as a newer sqi-server would have left it.
func bumpVersion(t *testing.T, path string) int64 {
	t.Helper()
	st, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return storetest.RecordNewerSchema(t, db, "sqlite")
}

func TestOpen_RefusesANewerSchema(t *testing.T) {
	for _, auto := range []bool{true, false} {
		path := t.TempDir() + "/test.db"
		bumpVersion(t, path)
		_, err := sqlite.Open(t.Context(), path, sqlite.Options{AutoMigrate: auto})
		if !errors.Is(err, migrations.ErrSchemaTooNew) {
			t.Fatalf("Open(AutoMigrate=%v) on a newer schema = %v, want ErrSchemaTooNew", auto, err)
		}
	}
}

func TestOpen_RefusingANewerSchemaReleasesTheServerLock(t *testing.T) {
	path := t.TempDir() + "/test.db"
	bumpVersion(t, path)
	if _, err := sqlite.Open(t.Context(), path, sqlite.Options{ServerLock: true}); !errors.Is(err, migrations.ErrSchemaTooNew) {
		t.Fatalf("Open = %v, want ErrSchemaTooNew", err)
	}
	l, err := sqlite.AcquireLock(path)
	if err != nil {
		t.Fatalf("lock still held after a refused Open: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaVersion(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, err := sqlite.SchemaVersion(t.Context(), db); err != nil || v != 0 {
		t.Fatalf("unmigrated SchemaVersion = %d, %v; want 0, nil", v, err)
	}
	want := bumpVersion(t, path)
	if v, err := sqlite.SchemaVersion(t.Context(), db); err != nil || v != want {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, want)
	}
}

func TestBackupPath_WorksOnANewerSchema(t *testing.T) {
	path := t.TempDir() + "/test.db"
	bumpVersion(t, path)
	out := t.TempDir() + "/backup.db"
	if err := sqlite.BackupPath(t.Context(), path, out); err != nil {
		t.Fatalf("BackupPath on a newer schema: %v (backup must work before a rollback)", err)
	}
}
