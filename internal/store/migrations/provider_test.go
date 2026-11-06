// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"io/fs"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/uberware/sqi/internal/store/migrations"
)

// Version N is the same schema on both backends, so every migration after the
// PostgreSQL baseline ships as a pair: the two sets hold the same versions from
// the baseline on, with no gap.
func TestPostgresFS_HoldsTheBaselineAndEveryLaterMigrationOfTheSQLiteSet(t *testing.T) {
	const baseline = 37
	versions := func(fsys fs.FS) []int64 {
		t.Helper()
		names, err := fs.Glob(fsys, "*.sql")
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, n := range names {
			v, err := goose.NumericComponent(n)
			if err != nil {
				t.Fatal(err)
			}
			if v >= baseline {
				out = append(out, v)
			}
		}
		slices.Sort(out)
		return out
	}
	pg, lite := versions(migrations.PostgresFS), versions(migrations.FS)
	if len(pg) == 0 || pg[0] != baseline {
		t.Fatalf("PostgresFS versions = %v, want them to start at the baseline %d", pg, baseline)
	}
	if !slices.Equal(pg, lite) {
		t.Fatalf("PostgresFS versions = %v, SQLite versions from %d = %v; a migration must ship in both", pg, baseline, lite)
	}
	for i, v := range pg {
		if v != baseline+int64(i) {
			t.Fatalf("versions %v have a gap at %d", pg, v)
		}
	}
	names, err := fs.Glob(migrations.PostgresFS, "00037_baseline.sql")
	if err != nil || len(names) != 1 {
		t.Fatalf("PostgresFS lacks 00037_baseline.sql: %v, %v", names, err)
	}
}

func TestNewProvider_SQLiteUpReachesTheLatestVersion(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := migrations.NewProvider(db, "sqlite")
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	want, err := migrations.MaxVersion("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.GetDBVersion(t.Context())
	if err != nil || v != want {
		t.Fatalf("version = %d, %v; want %d", v, err, want)
	}
}

func TestNewProvider_RejectsAnUnknownDriver(t *testing.T) {
	if _, err := migrations.NewProvider(nil, "mysql"); err == nil {
		t.Fatal("NewProvider accepted driver mysql")
	}
}

func TestMigration00041_DownRemovesTheHATablesAndColumns(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := migrations.NewProvider(db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(t.Context(), q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const tables = `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('server_lease', 'server_epoch', 'server_members')`
	const column = `SELECT count(*) FROM pragma_table_info('workers') WHERE name IN ('registration_broker_at', 'heartbeat_broker_at')`
	if count(tables) != 3 || count(column) != 2 {
		t.Fatalf("after Up: %d of 3 tables, %d of 2 columns", count(tables), count(column))
	}
	if _, err := p.DownTo(t.Context(), 40); err != nil {
		t.Fatalf("DownTo(40): %v", err)
	}
	if count(tables) != 0 || count(column) != 0 {
		t.Fatalf("after Down: %d tables and %d columns remain", count(tables), count(column))
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}

func TestMigration00042_DownRemovesTheProgressColumns(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := migrations.NewProvider(db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		var n int
		const q = `SELECT count(*) FROM pragma_table_info('task_attempts') WHERE name IN ('progress', 'status_text', 'progress_at')`
		if err := db.QueryRowContext(t.Context(), q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 3 {
		t.Fatalf("after Up: %d of 3 columns", n)
	}
	var dflt string
	if err := db.QueryRowContext(t.Context(), `SELECT dflt_value FROM pragma_table_info('task_attempts') WHERE name = 'status_text'`).Scan(&dflt); err != nil || dflt != "''" {
		t.Fatalf("status_text default = %q, %v; want ''", dflt, err)
	}
	if _, err := p.DownTo(t.Context(), 41); err != nil {
		t.Fatalf("DownTo(41): %v", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("after Down: %d columns remain", n)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if n := count(); n != 3 {
		t.Fatalf("after Up again: %d of 3 columns", n)
	}
}
