// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

import (
	"context"
	"database/sql"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/storecopy"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestStoreCopy_SQLiteToPostgres executes the runbook in docs/operations.md,
// "Moving to PostgreSQL": a farm runs on SQLite, the server stops, `store copy` moves it into
// PostgreSQL, and the server starts on PostgreSQL with the same NATS directory
// and carries on — including the attempt that was running when it stopped.
//
// It compares the two databases independently of the copy's own digest, but
// only coarsely: the same set of tables, the same row count per table (plain
// SELECT count(*)), and the same status for every job and task as each store
// reports them. The farm it runs fills 8 of the 20 tables. Value-by-value
// equality over every table, each one seeded, is
// internal/store/storecopy's TestStoreCopy_CopiesEveryRowVerbatim
// (assertRowsEqual).
//
// It needs SQLite as the source and a PostgreSQL target, so it ignores
// SQI_TEST_STORE_DRIVER and runs whenever PostgreSQL is configured.
func TestStoreCopy_SQLiteToPostgres(t *testing.T) {
	if !storetest.PostgresEnabled() {
		t.Skip("PostgreSQL not configured: set SQI_TEST_POSTGRES_DSN, or run make test-postgres")
	}
	src := filepath.Join(t.TempDir(), "sqi-source.db")
	ts := startServerWith(t, func(c *server.Config) {
		applyStore(c, backend.Options{Driver: backend.DriverSQLite, SQLitePath: src})
	})
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRestartableWorker(t, ts, farmID, queueID)

	finished := submitJobCustomYAML(t, ts, farmID, queueID, sleepJob)
	waitJobCompleted(t, ts, finished, 2*time.Minute)
	waitJobLogs(t, ts, finished)
	// Running when the server stops: the copy is exact and the NATS directory
	// is kept, so its attempt must carry on and its report land.
	inFlight := submitJobCustomYAML(t, ts, farmID, queueID, sleepJob)
	pollTaskStatus(t, ts, firstTaskID(t, ts, inFlight), []string{"running"}, 30*time.Second)
	ts.Stop(t)

	dst := storetest.PostgresEmptyDSN(t)
	res, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if err != nil {
		t.Fatalf("store copy: %v", err)
	}
	t.Logf("store copy: %d tables in %s", len(res.Tables), res.Elapsed)

	sqliteDB := openSQL(t, backend.Options{Driver: backend.DriverSQLite, SQLitePath: src})
	defer sqliteDB.Close()
	pgDB := openSQL(t, backend.Options{Driver: backend.DriverPostgres, PostgresDSN: dst})
	defer pgDB.Close()
	assertSameRowCounts(t, sqliteDB, pgDB)
	assertSameStatuses(t,
		backend.Options{Driver: backend.DriverSQLite, SQLitePath: src},
		backend.Options{Driver: backend.DriverPostgres, PostgresDSN: dst})

	pg := backend.Options{Driver: backend.DriverPostgres, PostgresDSN: dst, MaxConns: 8}
	applyStore(&ts.cfg, pg)
	ts.Store = pg
	ts = restartServer(t, ts)

	waitJobCompleted(t, ts, inFlight, 2*time.Minute)
	if got := jobStatus(t, ts, finished); got != "completed" {
		t.Fatalf("job finished on SQLite is %q on PostgreSQL, want completed", got)
	}
	assertNoInFlightTasks(t, ts)
}

// assertSameRowCounts compares the two databases' sets of tables and each
// table's row count with plain queries, independently of the copy's own
// digest. It also requires the tables a farm that has run jobs must fill.
func assertSameRowCounts(t *testing.T, sqliteDB, pgDB *sql.DB) {
	t.Helper()
	ctx := t.Context()
	srcTables := queryStrings(ctx, t, sqliteDB, `SELECT name FROM sqlite_master
WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name <> 'goose_db_version' ORDER BY name`)
	dstTables := queryStrings(ctx, t, pgDB, `SELECT table_name FROM information_schema.tables
WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name <> 'goose_db_version'
ORDER BY table_name`)
	if !slices.Equal(srcTables, dstTables) {
		t.Fatalf("tables differ:\n  sqlite:   %v\n  postgres: %v", srcTables, dstTables)
	}
	counts := map[string]int64{}
	for _, table := range srcTables {
		q := `SELECT count(*) FROM "` + table + `"`
		var src, dst int64
		if err := sqliteDB.QueryRowContext(ctx, q).Scan(&src); err != nil {
			t.Fatalf("sqlite %s: %v", q, err)
		}
		if err := pgDB.QueryRowContext(ctx, q).Scan(&dst); err != nil {
			t.Fatalf("postgres %s: %v", q, err)
		}
		if src != dst {
			t.Errorf("table %s: %d rows in SQLite, %d in PostgreSQL", table, src, dst)
		}
		counts[table] = src
	}
	for _, table := range []string{"farms", "queues", "workers", "jobs", "steps", "tasks", "task_attempts", "task_logs"} {
		if counts[table] == 0 {
			t.Errorf("table %s has no rows; the farm this test ran should have filled it", table)
		}
	}
	t.Logf("row counts: %v", counts)
}

// queryStrings runs q and returns its single string column.
func queryStrings(ctx context.Context, t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertSameStatuses compares every job's and every task's status as the two
// stores report them through the store interface.
func assertSameStatuses(t *testing.T, a, b backend.Options) {
	t.Helper()
	sa, sb := statuses(t, a), statuses(t, b)
	if len(sa) == 0 {
		t.Fatal("the source store lists no jobs or tasks")
	}
	if !maps.Equal(sa, sb) {
		t.Fatalf("statuses differ:\n  sqlite:   %v\n  postgres: %v", sa, sb)
	}
}

// statuses opens o's store and returns "job/<id>" and "task/<id>" mapped to
// their statuses.
func statuses(t *testing.T, o backend.Options) map[string]string {
	t.Helper()
	o.MaxConns = 2
	st, err := backend.Open(t.Context(), o)
	if err != nil {
		t.Fatalf("open %s store: %v", o.Driver, err)
	}
	defer st.Close()
	out := map[string]string{}
	jobs, err := st.ListJobs(t.Context(), store.ListJobsOptions{Pagination: store.Pagination{Limit: store.MaxLimit}})
	if err != nil {
		t.Fatalf("%s ListJobs: %v", o.Driver, err)
	}
	for _, j := range jobs.Items {
		out["job/"+j.ID] = string(j.Status)
		tasks, err := st.ListTasks(t.Context(), store.ListTasksOptions{
			JobID: j.ID, Pagination: store.Pagination{Limit: store.MaxLimit},
		})
		if err != nil {
			t.Fatalf("%s ListTasks: %v", o.Driver, err)
		}
		for _, task := range tasks.Items {
			out["task/"+task.ID] = string(task.Status)
		}
	}
	return out
}
