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
	want := []string{"00037_baseline.sql", "00038_task_logs_unique_seq.sql"}
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
	if err != nil || v != 38 {
		t.Fatalf("version = %d, %v; want 38", v, err)
	}
}

func TestNewProvider_RejectsAnUnknownDriver(t *testing.T) {
	if _, err := migrations.NewProvider(nil, "mysql"); err == nil {
		t.Fatal("NewProvider accepted driver mysql")
	}
}
