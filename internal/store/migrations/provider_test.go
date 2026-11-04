// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"io/fs"
	"slices"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/uberware/sqi/internal/store/migrations"
)

func TestPostgresFS_HoldsTheBaselineAndLaterMigrations(t *testing.T) {
	names, err := fs.Glob(migrations.PostgresFS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"00037_baseline.sql", "00038_task_logs_unique_seq.sql", "00039_worker_extensions.sql", "00040_broker_state.sql", "00041_server_ha.sql"}
	if !slices.Equal(names, want) {
		t.Fatalf("PostgresFS = %v, want %v", names, want)
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
	v, err := p.GetDBVersion(t.Context())
	if err != nil || v != 41 {
		t.Fatalf("version = %d, %v; want 41", v, err)
	}
}

func TestNewProvider_RejectsAnUnknownDriver(t *testing.T) {
	if _, err := migrations.NewProvider(nil, "mysql"); err == nil {
		t.Fatal("NewProvider accepted driver mysql")
	}
}

func TestMigration00041_DownRemovesTheHATablesAndColumn(t *testing.T) {
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
	const column = `SELECT count(*) FROM pragma_table_info('workers') WHERE name = 'registration_broker_at'`
	if count(tables) != 3 || count(column) != 1 {
		t.Fatalf("after Up: %d of 3 tables, %d of 1 column", count(tables), count(column))
	}
	if _, err := p.DownTo(t.Context(), 40); err != nil {
		t.Fatalf("DownTo(40): %v", err)
	}
	if count(tables) != 0 || count(column) != 0 {
		t.Fatalf("after Down: %d tables and %d column remain", count(tables), count(column))
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
