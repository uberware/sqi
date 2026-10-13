// SPDX-License-Identifier: AGPL-3.0-or-later

package schemainfo_test

import (
	"database/sql"
	"maps"
	"slices"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/schemainfo"
	"github.com/uberware/sqi/internal/store/storetest"
)

// migratedSQLite returns a fresh SQLite database migrated to the newest schema.
func migratedSQLite(t *testing.T) *sql.DB {
	t.Helper()
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
	return db
}

func TestSQLite_ReadsTheMigratedSchema(t *testing.T) {
	info, err := schemainfo.SQLite(t.Context(), migratedSQLite(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"api_keys", "audit_log", "compute_locations", "farms", "job_dependencies", "jobs", "products",
		"queues", "sessions", "steps", "storage_locations", "task_attempts", "task_logs", "tasks",
		"usage_claims", "usage_pools", "users", "worker_credentials", "worker_join_tokens", "workers",
	}
	if got := slices.Sorted(maps.Keys(info)); !slices.Equal(got, want) {
		t.Fatalf("tables = %v\nwant     %v", got, want)
	}
	if got := info["job_dependencies"].PrimaryKey; !slices.Equal(got, []string{"job_id", "depends_on_job_id"}) {
		t.Errorf("job_dependencies primary key = %v", got)
	}
	if got := info["tasks"].PrimaryKey; !slices.Equal(got, []string{"id"}) {
		t.Errorf("tasks primary key = %v", got)
	}
	if got := info["tasks"].References; !slices.Equal(got, []string{"jobs", "steps"}) {
		t.Errorf("tasks references = %v, want [jobs steps]", got)
	}
	if got := info["usage_claims"].References; !slices.Equal(got, []string{"task_attempts", "usage_pools"}) {
		t.Errorf("usage_claims references = %v", got)
	}
	if got := info["farms"].References; len(got) != 0 {
		t.Errorf("farms references = %v, want none", got)
	}
	cols := info["queues"].Columns
	if len(cols) == 0 || cols[0].Name != "id" {
		t.Fatalf("queues columns = %v, want id first", cols)
	}
	for _, c := range cols {
		if c.Type != strings.ToLower(c.Type) {
			t.Errorf("queues.%s type %q is not lower-cased", c.Name, c.Type)
		}
		if c.Name == "paused" && c.Type != "integer" {
			t.Errorf("queues.paused type = %q, want integer (SQLite has no boolean)", c.Type)
		}
	}
}

func TestInsertOrder_ParentsFirst(t *testing.T) {
	info, err := schemainfo.SQLite(t.Context(), migratedSQLite(t))
	if err != nil {
		t.Fatal(err)
	}
	order, err := schemainfo.InsertOrder(info)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != len(info) {
		t.Fatalf("order has %d tables, schema %d", len(order), len(info))
	}
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	for _, pair := range [][2]string{
		{"farms", "queues"},
		{"tasks", "task_attempts"},
		{"task_attempts", "task_logs"},
		{"jobs", "steps"},
		{"steps", "tasks"},
		{"usage_pools", "usage_claims"},
		{"users", "api_keys"},
	} {
		if pos[pair[0]] >= pos[pair[1]] {
			t.Errorf("%s at %d, %s at %d; want the parent first (order %v)", pair[0], pos[pair[0]], pair[1], pos[pair[1]], order)
		}
	}
	// Every table comes after everything it references.
	for n, tbl := range info {
		for _, r := range tbl.References {
			if pos[r] >= pos[n] {
				t.Errorf("%s references %s but comes first", n, r)
			}
		}
	}
	again, err := schemainfo.InsertOrder(info)
	if err != nil || !slices.Equal(again, order) {
		t.Errorf("InsertOrder is not stable: %v then %v (%v)", order, again, err)
	}
}

func TestInsertOrder_RefusesACycle(t *testing.T) {
	_, err := schemainfo.InsertOrder(map[string]schemainfo.Table{
		"a": {Name: "a", References: []string{"b"}},
		"b": {Name: "b", References: []string{"a"}},
		"c": {Name: "c"},
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("InsertOrder over a cycle = %v, want a cycle error", err)
	}
}

func TestInsertOrder_RefusesAnUnknownReference(t *testing.T) {
	_, err := schemainfo.InsertOrder(map[string]schemainfo.Table{"a": {Name: "a", References: []string{"gone"}}})
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("InsertOrder with a dangling reference = %v, want it named", err)
	}
}

// TestPostgres_ReadsAMixedCaseSchema reads tables from a current schema whose
// name needs quoting as an identifier: the name current_schema() returns must
// be compared as a name, not re-parsed as an identifier (which folds case).
func TestPostgres_ReadsAMixedCaseSchema(t *testing.T) {
	if !storetest.PostgresEnabled() {
		t.Skip("PostgreSQL not configured: set SQI_TEST_POSTGRES_DSN, or run make test-postgres")
	}
	db, err := sql.Open("pgx", storetest.PostgresEmptyDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1) // the search_path below is per session
	for _, q := range []string{
		`CREATE SCHEMA "Farm"`,
		`CREATE TABLE "Farm".parent (id text PRIMARY KEY)`,
		`CREATE TABLE "Farm".child (id text PRIMARY KEY, parent_id text REFERENCES "Farm".parent (id))`,
		`CREATE TABLE public.elsewhere (id text PRIMARY KEY)`,
		`SET search_path TO "Farm"`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	info, err := schemainfo.Postgres(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(info)); !slices.Equal(got, []string{"child", "parent"}) {
		t.Fatalf("tables = %v, want [child parent] from schema \"Farm\"", got)
	}
	if got := info["child"].References; !slices.Equal(got, []string{"parent"}) {
		t.Errorf("child references = %v", got)
	}
	if got := info["child"].PrimaryKey; !slices.Equal(got, []string{"id"}) {
		t.Errorf("child primary key = %v", got)
	}
}
