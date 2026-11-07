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

func TestMigration00043_DownRemovesTheEstimateColumns(t *testing.T) {
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
	const names = `('estimated_seconds', 'product_name', 'product_version')`
	count := func(extra string) int {
		t.Helper()
		var n int
		q := `SELECT count(*) FROM pragma_table_info('jobs') WHERE name IN ` + names + extra
		if err := db.QueryRowContext(t.Context(), q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(""); n != 3 {
		t.Fatalf("after Up: %d of 3 columns", n)
	}
	if n := count(" AND \"notnull\" = 0 AND dflt_value IS NULL"); n != 3 {
		t.Fatalf("after Up: %d of 3 columns nullable without default", n)
	}
	if _, err := p.DownTo(t.Context(), 42); err != nil {
		t.Fatalf("DownTo(42): %v", err)
	}
	if n := count(""); n != 0 {
		t.Fatalf("after Down: %d columns remain", n)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if n := count(""); n != 3 {
		t.Fatalf("after Up again: %d of 3 columns", n)
	}
}

// taskAttemptsIndexes lists the names of the indexes on task_attempts.
func taskAttemptsIndexes(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// checkMigration00044 drives 00044 up, down and up again, asserting the index
// set on task_attempts at each point.
func checkMigration00044(t *testing.T, db *sql.DB, driver, query string) {
	t.Helper()
	p, err := migrations.NewProvider(db, driver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	has := func(name string) bool {
		return slices.Contains(taskAttemptsIndexes(t, db, query), name)
	}
	if has("task_attempts_task_id") || !has("task_attempts_task_num") {
		t.Fatalf("after Up: indexes %v; want task_num kept, task_id dropped", taskAttemptsIndexes(t, db, query))
	}
	if !has("task_attempts_worker_id") || !has("task_attempts_session_id") {
		t.Fatalf("after Up: indexes %v; want worker_id and session_id kept", taskAttemptsIndexes(t, db, query))
	}
	if _, err := p.DownTo(t.Context(), 43); err != nil {
		t.Fatalf("DownTo(43): %v", err)
	}
	if !has("task_attempts_task_id") || has("task_attempts_task_num") {
		t.Fatalf("after Down: indexes %v; want task_id restored, task_num gone", taskAttemptsIndexes(t, db, query))
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if has("task_attempts_task_id") || !has("task_attempts_task_num") {
		t.Fatalf("after Up again: indexes %v", taskAttemptsIndexes(t, db, query))
	}
}

func TestMigration00044_ReplacesTheTaskIDIndexAndDownRestoresIt(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	checkMigration00044(t, db, "sqlite", `SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'task_attempts'`)
}
