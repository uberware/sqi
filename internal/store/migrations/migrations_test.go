// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/sqlite"
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

// TestMigration00024_ExternalIDRoundTrip pins that 00024_users_external_id
// applies cleanly against a fresh database and that the new column round-trips
// through CreateUser.
func TestMigration00024_ExternalIDRoundTrip(t *testing.T) {
	db := t.TempDir() + "/test.db"
	st, err := sqlite.Open(t.Context(), db, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := t.Context()
	u, err := st.CreateUser(ctx, store.User{
		ID: "u1", Username: "alice", PasswordHash: "!ldap",
		Role: "user", AuthSource: store.AuthSourceLDAP, ExternalID: "guid-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ExternalID != "guid-1" {
		t.Fatalf("ExternalID = %q, want %q", u.ExternalID, "guid-1")
	}
}

// TestMigrations_00025_QueueRunAsUserDownUp pins that 00025_queue_run_as_user
// applies cleanly, that its Down migration actually drops both new columns
// (the tricky direction: SQLite's ALTER TABLE DROP COLUMN is refused when a
// CHECK constraint references the column, which is exactly why this
// migration deliberately carries none), and that re-applying Up restores
// them.
func TestMigrations_00025_QueueRunAsUserDownUp(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	if !hasColumn(t, db, "queues", "run_as_user") {
		t.Fatal("run_as_user column missing after Up")
	}
	if !hasColumn(t, db, "queues", "run_as_group") {
		t.Fatal("run_as_group column missing after Up")
	}

	if err := goose.DownTo(db, ".", 24); err != nil {
		t.Fatalf("goose.DownTo(24): %v", err)
	}
	if hasColumn(t, db, "queues", "run_as_user") {
		t.Fatal("run_as_user column still present after Down")
	}
	if hasColumn(t, db, "queues", "run_as_group") {
		t.Fatal("run_as_group column still present after Down")
	}

	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up (re-apply): %v", err)
	}
	if !hasColumn(t, db, "queues", "run_as_user") {
		t.Fatal("run_as_user column missing after re-Up")
	}
	if !hasColumn(t, db, "queues", "run_as_group") {
		t.Fatal("run_as_group column missing after re-Up")
	}
}

// hasColumn reports whether table has a column named column.
func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info row: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	return false
}
