// SPDX-License-Identifier: AGPL-3.0-or-later

package restore_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/restore"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

var fixedNow = time.Date(2026, 10, 12, 15, 30, 0, 0, time.UTC)

const stamp = ".pre-restore-20261012T153000Z"

// liveFarm makes a migrated SQLite at dir/sqi.db holding a job with one running
// task, a NATS dir with a directory in it, and a backup of it at dir/backup.db.
func liveFarm(t *testing.T) (dir, db, natsDir, backup, taskID string) {
	t.Helper()
	dir = t.TempDir()
	db = filepath.Join(dir, "sqi.db")
	st, err := sqlite.Open(t.Context(), db, sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
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
	taskID = "t1"
	backup = filepath.Join(dir, "backup.db")
	if err := st.Backup(t.Context(), backup); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	natsDir = filepath.Join(dir, "nats")
	if err := os.MkdirAll(filepath.Join(natsDir, "jetstream"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, db, natsDir, backup, taskID
}

func opts(db, natsDir, backup string) restore.Options {
	return restore.Options{
		Driver: backend.DriverSQLite, SQLitePath: db, NATSDataDir: natsDir, Backup: backup,
		Now: func() time.Time { return fixedNow },
	}
}

// bumpFile records one more applied schema version in the SQLite file at path
// than this binary knows, as a newer sqi-server would have left it.
func bumpFile(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	maxV, err := migrations.MaxVersion("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, maxV+1); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreSQLite_PutsTheBackupInPlaceFencesAndQuarantines(t *testing.T) {
	_, db, natsDir, backup, taskID := liveFarm(t)
	res, err := restore.Run(t.Context(), opts(db, natsDir, backup))
	if err != nil {
		t.Fatal(err)
	}
	maxV, err := migrations.MaxVersion("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupVersion != maxV || res.Version != maxV || res.Fenced != 1 {
		t.Fatalf("result = %+v", res)
	}
	if want := db + stamp; !slices.Contains(res.MovedAside, want) {
		t.Fatalf("MovedAside = %v, want it to include %s", res.MovedAside, want)
	}
	if res.NATSMovedTo != natsDir+stamp {
		t.Fatalf("NATSMovedTo = %q", res.NATSMovedTo)
	}
	if _, err := os.Stat(natsDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NATS dir still at %s: %v", natsDir, err)
	}
	if _, err := os.Stat(filepath.Join(natsDir+stamp, "jetstream")); err != nil {
		t.Fatalf("NATS dir contents not moved: %v", err)
	}
	if _, err := os.Stat(db + ".restore-tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file left behind: %v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("the backup file itself is gone: %v", err)
	}
	st, err := sqlite.Open(t.Context(), db, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task, err := st.GetTask(t.Context(), taskID)
	if err != nil || task.Status != store.TaskStatusReady {
		t.Fatalf("restored task = %+v, %v; want ready (fenced)", task, err)
	}
}

func TestRestoreSQLite_RefusesWhileAServerHoldsTheLock(t *testing.T) {
	_, db, natsDir, backup, _ := liveFarm(t)
	srv, err := sqlite.Open(t.Context(), db, sqlite.Options{ServerLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if _, err := restore.Run(t.Context(), opts(db, natsDir, backup)); !errors.Is(err, sqlite.ErrServerLockHeld) {
		t.Fatalf("Run beside a server = %v, want ErrServerLockHeld", err)
	}
	if _, err := os.Stat(natsDir); err != nil {
		t.Fatal("NATS dir moved although the restore was refused")
	}
	if _, err := os.Stat(db + stamp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the live database was moved aside although the restore was refused")
	}
}

func TestRestoreSQLite_RefusesANewerBackup(t *testing.T) {
	_, db, natsDir, backup, _ := liveFarm(t)
	bumpFile(t, backup)
	_, err := restore.Run(t.Context(), opts(db, natsDir, backup))
	if !errors.Is(err, migrations.ErrSchemaTooNew) {
		t.Fatalf("Run with a newer backup = %v, want ErrSchemaTooNew", err)
	}
	if _, err := os.Stat(db + stamp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the live database was moved aside before the backup was checked")
	}
	if _, err := os.Stat(db + ".restore-tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file left behind: %v", err)
	}
}

func TestRestoreSQLite_RefusesAFileThatIsNotAnSQIBackup(t *testing.T) {
	_, db, natsDir, _, _ := liveFarm(t)
	junk := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(junk, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restore.Run(t.Context(), opts(db, natsDir, junk)); !errors.Is(err, restore.ErrNotSQIBackup) {
		t.Fatalf("Run with junk = %v, want ErrNotSQIBackup", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restore.Run(t.Context(), opts(db, natsDir, empty)); !errors.Is(err, restore.ErrNotSQIBackup) {
		t.Fatalf("Run with an empty file = %v, want ErrNotSQIBackup", err)
	}
	if _, err := os.Stat(db + stamp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the live database was moved aside for a file that is not a backup")
	}
}

// Review focus 1: naming the live database as the backup would move it aside
// and then find nothing to put in its place.
func TestRestoreSQLite_RefusesTheLiveDatabaseAsBackup(t *testing.T) {
	_, db, natsDir, _, _ := liveFarm(t)
	_, err := restore.Run(t.Context(), opts(db, natsDir, db))
	if err == nil || !strings.Contains(err.Error(), "is the live database") {
		t.Fatalf("Run with the live path as backup = %v, want a refusal", err)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("the live database is gone: %v", err)
	}
}

// Review focus 2: an absent NATS directory is reported by the absolute path
// looked at, so an operator can spot a wrong nats.data_dir.
func TestRestore_ReportsAMissingNATSDirByAbsolutePath(t *testing.T) {
	_, db, _, backup, _ := liveFarm(t)
	missing := filepath.Join(t.TempDir(), "nowhere")
	res, err := restore.Run(t.Context(), opts(db, missing, backup))
	if err != nil {
		t.Fatal(err)
	}
	if res.NATSMovedTo != "" || !filepath.IsAbs(res.NATSDir) || res.NATSDir != missing {
		t.Fatalf("result = %+v; want NATSDir=%s and nothing moved", res, missing)
	}
}

// Review focus 3: once the live database is moved aside, any failure must say
// where it went.
func TestRestoreSQLite_FailureAfterMoveAsideNamesThePreviousDatabase(t *testing.T) {
	_, db, natsDir, backup, _ := liveFarm(t)
	restore.SetRenameForTest(t, func(from, to string) error {
		if strings.HasSuffix(from, ".restore-tmp") {
			return errors.New("injected")
		}
		return os.Rename(from, to)
	})
	_, err := restore.Run(t.Context(), opts(db, natsDir, backup))
	if err == nil || !strings.Contains(err.Error(), db+stamp) {
		t.Fatalf("err = %v; want it to name where the previous database is", err)
	}
	if _, err := os.Stat(natsDir); err != nil {
		t.Fatal("NATS dir moved although the restore failed")
	}
}

func TestRestore_InPlaceFencesTheConfiguredDatabase(t *testing.T) {
	_, db, natsDir, _, taskID := liveFarm(t)
	o := opts(db, natsDir, "")
	o.InPlace = true
	res, err := restore.Run(t.Context(), o)
	if err != nil || res.Fenced != 1 || res.BackupVersion != 0 || len(res.MovedAside) != 0 {
		t.Fatalf("in-place = %+v, %v", res, err)
	}
	if res.NATSMovedTo != natsDir+stamp {
		t.Fatalf("NATSMovedTo = %q", res.NATSMovedTo)
	}
	st, err := sqlite.Open(context.Background(), db, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if task, err := st.GetTask(t.Context(), taskID); err != nil || task.Status != store.TaskStatusReady {
		t.Fatalf("task = %+v, %v; want ready", task, err)
	}
}

func TestRestore_NeedsExactlyOneOfBackupAndInPlace(t *testing.T) {
	_, db, natsDir, backup, _ := liveFarm(t)
	for _, o := range []restore.Options{opts(db, natsDir, ""), func() restore.Options {
		o := opts(db, natsDir, backup)
		o.InPlace = true
		return o
	}(), opts("", natsDir, backup)} {
		if _, err := restore.Run(t.Context(), o); err == nil {
			t.Fatalf("Run(%+v) = nil, want an error", o)
		}
	}
}
