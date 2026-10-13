// SPDX-License-Identifier: AGPL-3.0-or-later

package restore_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/restore"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

// pgFarm makes a migrated PostgreSQL database holding a job with one running
// task, and returns its DSN and an open store on it. When bump is set, one more
// applied schema version than this binary knows is recorded first, as a newer
// sqi-server would have left it.
func pgFarm(t *testing.T, bump bool) (dsn string, st *postgres.Store) {
	t.Helper()
	storetest.RequirePostgresTool(t, "pg_dump")
	storetest.RequirePostgresTool(t, "pg_restore")
	dsn = storetest.PostgresDSN(t)
	st, err := postgres.Open(t.Context(), dsn, postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	farm, err := st.CreateFarm(t.Context(), store.Farm{ID: "f1", Name: "restore-farm"})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := st.CreateQueue(t.Context(), store.Queue{ID: "q1", FarmID: farm.ID, Name: "restore-queue"})
	if err != nil {
		t.Fatal(err)
	}
	storetest.NewJob("j1", farm.ID, queue.ID).
		Step("s1", store.StepStatusReady).
		Task("t1", "s1", store.TaskStatusRunning).
		SubmitLeasing(t, st, "w1")
	if bump {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		maxV, err := migrations.MaxVersion("postgres")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(),
			`INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, maxV+1); err != nil {
			t.Fatal(err)
		}
	}
	return dsn, st
}

func pgOpts(dsn, backupFile string) restore.Options {
	return restore.Options{
		Driver: backend.DriverPostgres, PostgresDSN: dsn, Backup: backupFile,
		Now: func() time.Time { return fixedNow },
	}
}

func TestRestorePostgres_FencesAndRefusesANonEmptyTarget(t *testing.T) {
	_, src := pgFarm(t, false)
	archive := filepath.Join(t.TempDir(), "sqi.dump")
	if err := src.Backup(t.Context(), archive); err != nil {
		t.Fatal(err)
	}
	dst := storetest.PostgresEmptyDSN(t)
	res, err := restore.Run(t.Context(), pgOpts(dst, archive))
	if err != nil {
		t.Fatal(err)
	}
	maxV, err := migrations.MaxVersion("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupVersion != maxV || res.Version != maxV || res.Fenced != 1 || len(res.MovedAside) != 0 {
		t.Fatalf("result = %+v", res)
	}
	got, err := postgres.Open(t.Context(), dst, postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if task, err := got.GetTask(t.Context(), "t1"); err != nil || task.Status != store.TaskStatusReady {
		t.Fatalf("restored task = %+v, %v; want ready (fenced)", task, err)
	}

	// A second restore into the same, now populated, database is refused.
	_, err = restore.Run(t.Context(), pgOpts(dst, archive))
	if err == nil || !strings.Contains(err.Error(), "needs an empty database") {
		t.Fatalf("second Run = %v, want a refusal naming the empty-database rule", err)
	}
}

func TestRestorePostgres_RefusesANewerBackupWithoutTouchingTheTarget(t *testing.T) {
	_, src := pgFarm(t, true)
	archive := filepath.Join(t.TempDir(), "sqi.dump")
	if err := src.Backup(t.Context(), archive); err != nil {
		t.Fatal(err)
	}
	dst := storetest.PostgresEmptyDSN(t)
	if _, err := restore.Run(t.Context(), pgOpts(dst, archive)); !errors.Is(err, migrations.ErrSchemaTooNew) {
		t.Fatalf("Run with a newer backup = %v, want ErrSchemaTooNew", err)
	}
	if err := postgres.RequireEmptyDatabase(t.Context(), dst); err != nil {
		t.Fatalf("the target was touched: %v", err)
	}
}

func TestRestorePostgres_RefusesAFileThatIsNotAnSQIBackup(t *testing.T) {
	storetest.RequirePostgresTool(t, "pg_restore")
	dst := storetest.PostgresEmptyDSN(t)
	junk := filepath.Join(t.TempDir(), "junk.dump")
	if err := os.WriteFile(junk, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restore.Run(t.Context(), pgOpts(dst, junk)); !errors.Is(err, restore.ErrNotSQIBackup) {
		t.Fatalf("Run with junk = %v, want ErrNotSQIBackup", err)
	}
	if err := postgres.RequireEmptyDatabase(t.Context(), dst); err != nil {
		t.Fatalf("the target was touched: %v", err)
	}
}

// A missing pg_restore is a tool problem: reporting it as "not an sqi backup"
// would send the operator looking at a good backup.
func TestRestorePostgres_AMissingPgRestoreIsNotABadBackup(t *testing.T) {
	dst := storetest.PostgresEmptyDSN(t)
	archive := filepath.Join(t.TempDir(), "sqi.dump")
	if err := os.WriteFile(archive, []byte("PGDMP"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	_, err := restore.Run(t.Context(), pgOpts(dst, archive))
	if err == nil || errors.Is(err, restore.ErrNotSQIBackup) || !strings.Contains(err.Error(), "pg_restore not found on PATH") {
		t.Fatalf("Run without pg_restore = %v, want a missing-tool error that is not ErrNotSQIBackup", err)
	}
	if err := postgres.RequireEmptyDatabase(t.Context(), dst); err != nil {
		t.Fatalf("the target was touched: %v", err)
	}
}

func TestRestorePostgres_InPlaceFencesTheConfiguredDatabase(t *testing.T) {
	dsn, st := pgFarm(t, false)
	o := pgOpts(dsn, "")
	o.InPlace = true
	res, err := restore.Run(t.Context(), o)
	if err != nil || res.Fenced != 1 || res.BackupVersion != 0 {
		t.Fatalf("in-place = %+v, %v", res, err)
	}
	if task, err := st.GetTask(t.Context(), "t1"); err != nil || task.Status != store.TaskStatusReady {
		t.Fatalf("task = %+v, %v; want ready", task, err)
	}
}

// --in-place on an empty database must refuse: opening it with AutoMigrate
// would create the schema and report a restore that restored nothing.
func TestRestorePostgres_InPlaceRefusesAnEmptyDatabase(t *testing.T) {
	dst := storetest.PostgresEmptyDSN(t)
	o := pgOpts(dst, "")
	o.InPlace = true
	_, err := restore.Run(t.Context(), o)
	if err == nil || !strings.Contains(err.Error(), "--in-place needs") {
		t.Fatalf("in-place on an empty database = %v, want a refusal", err)
	}
	if err := postgres.RequireEmptyDatabase(t.Context(), dst); err != nil {
		t.Fatalf("the empty database was migrated: %v", err)
	}
}
