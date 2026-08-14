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

// TestMigrations_00026_WorkerExprLimitsDownUp pins 00026_worker_expr_limits in
// its own right. Before this test the column was covered only INCIDENTALLY, by
// TestMigrations_00025_QueueRunAsUserDownUp's DownTo(24) passing through it --
// which asserts nothing about 00026 and would keep passing if 00026's Down
// silently dropped the wrong thing.
//
// Down is the direction worth pinning: SQLite refuses ALTER TABLE DROP COLUMN
// on a column referenced by a CHECK constraint or an index, so a later revision
// that adds either to expr_limits makes the Down un-runnable. The re-Up leg
// then proves the column comes back with its NOT NULL DEFAULT '{}' intact,
// which is what lets scanWorker read a pre-existing row as the zero struct
// rather than failing on a NULL.
func TestMigrations_00026_WorkerExprLimitsDownUp(t *testing.T) {
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
	if !hasColumn(t, db, "workers", "expr_limits") {
		t.Fatal("expr_limits column missing after Up")
	}

	// A row written before the Down must survive the Down/Up cycle, and the
	// re-added column must default to '{}' rather than NULL -- scanWorker reads
	// it into a plain string.
	if _, err := db.ExecContext(
		t.Context(),
		`INSERT INTO workers (id, hostname, os, status, registered_at, updated_at)
		 VALUES ('w-1', 'h', 'linux', 'offline', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("insert worker: %v", err)
	}

	if err := goose.DownTo(db, ".", 25); err != nil {
		t.Fatalf("goose.DownTo(25): %v", err)
	}
	if hasColumn(t, db, "workers", "expr_limits") {
		t.Fatal("expr_limits column still present after Down")
	}

	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up (re-apply): %v", err)
	}
	if !hasColumn(t, db, "workers", "expr_limits") {
		t.Fatal("expr_limits column missing after re-Up")
	}
	var exprLimits string
	if err := db.QueryRowContext(t.Context(),
		`SELECT expr_limits FROM workers WHERE id = 'w-1'`).Scan(&exprLimits); err != nil {
		t.Fatalf("select expr_limits after re-Up: %v (a NULL here is a scanWorker failure "+
			"for every pre-existing worker row)", err)
	}
	if exprLimits != "{}" {
		t.Errorf("expr_limits = %q for a row that predates the column, want %q", exprLimits, "{}")
	}
}

// TestMigrations_00027_JobDeclaredExtensionsDownUp pins 00027 in both
// directions, and pins the DEFAULT, which is the part of this migration that
// carries the meaning.
//
// A row that predates the column must read back as ” -- "not recorded" -- and
// NOT as '[]', which means "recorded, and declares nothing". The scheduler
// falls back to its raw-template byte scan for the first and skips the scan
// entirely for the second, so a default of '[]' would silently ungate every
// EXPR job submitted before the upgrade. That is the whole reason the column is
// TEXT with an empty-string default rather than a JSON list.
//
// Down is pinned for the same reason 00026's is: SQLite refuses ALTER TABLE
// DROP COLUMN on a column referenced by a CHECK constraint or an index, so a
// later revision that adds either makes the Down un-runnable.
func TestMigrations_00027_JobDeclaredExtensionsDownUp(t *testing.T) {
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
	if !hasColumn(t, db, "jobs", "declared_extensions") {
		t.Fatal("declared_extensions column missing after Up")
	}

	seedJobRowWithoutExtensions(t, db)

	if err := goose.DownTo(db, ".", 26); err != nil {
		t.Fatalf("goose.DownTo(26): %v", err)
	}
	if hasColumn(t, db, "jobs", "declared_extensions") {
		t.Fatal("declared_extensions column still present after Down")
	}

	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up (re-apply): %v", err)
	}
	if !hasColumn(t, db, "jobs", "declared_extensions") {
		t.Fatal("declared_extensions column missing after re-Up")
	}

	var declared string
	if err := db.QueryRowContext(t.Context(),
		`SELECT declared_extensions FROM jobs WHERE id = 'j-1'`).Scan(&declared); err != nil {
		t.Fatalf("select declared_extensions after re-Up: %v (a NULL here is a scanJob "+
			"failure for every pre-existing job row)", err)
	}
	if declared != "" {
		t.Errorf("declared_extensions = %q for a row that predates the column, want %q "+
			"(\"not recorded\"); %q would mean \"recorded, declares nothing\" and would "+
			"ungate every EXPR job submitted before the upgrade", declared, "", "[]")
	}
}

// seedJobRowWithoutExtensions inserts the farm, queue and job a pre-migration
// deployment would already hold, using raw SQL so no Go-side default can creep
// in and mask what the schema actually stores.
func seedJobRowWithoutExtensions(t *testing.T, db *sql.DB) {
	t.Helper()
	const ts = "2026-01-01T00:00:00Z"
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{
			`INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			[]any{"f-1", "farm", ts, ts},
		},
		{
			`INSERT INTO queues (id, farm_id, name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			[]any{"q-1", "f-1", "queue", ts, ts},
		},
		{
			`INSERT INTO jobs (id, farm_id, queue_id, name, status, raw_template, template_format,
			created_at, updated_at)
		  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{"j-1", "f-1", "q-1", "job", "pending", "{}", "json", ts, ts},
		},
	} {
		if _, err := db.ExecContext(t.Context(), stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed row (%s): %v", stmt.sql, err)
		}
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
