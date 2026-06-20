// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

// Regression test for migration 00012_relax_worker_fks.sql.
//
// The table rebuild must succeed on a *populated* database, not just an empty
// one. goose runs each migration in a transaction, where "PRAGMA foreign_keys"
// is a no-op; the migration therefore relies on "PRAGMA defer_foreign_keys" so
// that DROP TABLE tasks (an implicit row-delete) does not trip the foreign keys
// that task_attempts and task_logs hold on tasks. This test reproduces a real
// deployment: migrate to the pre-00012 schema, seed referencing rows, then
// migrate the rest of the way.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // register the "sqlite" driver

	"github.com/uberware/sqi/internal/store/migrations"
)

// versionBeforeRelaxFKs is the migration applied immediately before 00012.
const versionBeforeRelaxFKs = 11

func TestMigration00012_RelaxWorkerFKs_PopulatedDB(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/populated.db"

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Mirror the production connection setup so foreign keys are enforced.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err = db.ExecContext(ctx, pragma); err != nil {
			t.Fatalf("pragma %q: %v", pragma, err)
		}
	}

	goose.SetBaseFS(migrations.FS)
	if err = goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	// 1. Migrate to the schema just before the FK relaxation.
	if err = goose.UpTo(db, ".", versionBeforeRelaxFKs); err != nil {
		t.Fatalf("migrate up to %d: %v", versionBeforeRelaxFKs, err)
	}

	// 2. Seed a full chain so tasks has children: a task_attempt (FK to tasks +
	//    workers) and a task_log (FK to tasks + task_attempts).
	seed := []string{
		`INSERT INTO farms (id, name, created_at, updated_at) VALUES ('f1','F1','t','t')`,
		`INSERT INTO queues (id, farm_id, name, created_at, updated_at) VALUES ('q1','f1','Q1','t','t')`,
		`INSERT INTO jobs (id, farm_id, queue_id, name, created_at, updated_at) VALUES ('j1','f1','q1','J1','t','t')`,
		`INSERT INTO steps (id, job_id, name, created_at, updated_at) VALUES ('s1','j1','S1','t','t')`,
		`INSERT INTO workers (id, hostname, registered_at, updated_at) VALUES ('w1','h1','t','t')`,
		`INSERT INTO tasks (id, job_id, step_id, name, assigned_worker_id, created_at, updated_at) VALUES ('t1','j1','s1','T1','w1','t','t')`,
		`INSERT INTO task_attempts (id, task_id, worker_id, started_at, created_at) VALUES ('a1','t1','w1','t','t')`,
		`INSERT INTO task_logs (id, task_id, attempt_id, worker_seq, nats_seq, at, received_at) VALUES ('l1','t1','a1',1,1,'t','t')`,
	}
	for _, stmt := range seed {
		if _, err = db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// 3. Apply the remaining migrations (including 00012). This is the step that
	//    failed before the defer_foreign_keys fix.
	if err = goose.Up(db, "."); err != nil {
		t.Fatalf("migrate up (00012 on populated DB): %v", err)
	}

	// 4. The child rows must survive the rebuild.
	for _, c := range []struct {
		table string
		id    string
	}{{"tasks", "t1"}, {"task_attempts", "a1"}, {"task_logs", "l1"}} {
		var n int
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+c.table+" WHERE id = ?", c.id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if n != 1 {
			t.Errorf("%s row %q: got %d, want 1", c.table, c.id, n)
		}
	}

	// 5. With the FK relaxed, the worker can now be hard-deleted even though
	//    task_attempts still references it by id.
	if _, err = db.ExecContext(ctx, "DELETE FROM workers WHERE id = 'w1'"); err != nil {
		t.Fatalf("delete worker after relax: %v", err)
	}
	var attempts int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_attempts WHERE worker_id = 'w1'").Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempt worker_id snapshot: got %d rows, want 1 (history preserved)", attempts)
	}
}
