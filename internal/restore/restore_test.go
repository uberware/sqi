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

	"github.com/uberware/sqi/internal/fsutil"
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
// where it went: every file, since a database and its -wal are one database
// only together, and where the verified staged copy was left.
func TestRestoreSQLite_FailureAfterMoveAsideNamesThePreviousDatabase(t *testing.T) {
	_, db, natsDir, backup, _ := liveFarm(t)
	if err := os.WriteFile(db+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore.SetRenameForTest(t, func(from, to string) error {
		if strings.HasSuffix(from, ".restore-tmp") {
			return errors.New("injected")
		}
		return os.Rename(from, to)
	})
	_, err := restore.Run(t.Context(), opts(db, natsDir, backup))
	if err == nil {
		t.Fatal("Run with a failing rename = nil, want an error")
	}
	for _, want := range []string{db + stamp, db + "-wal" + stamp, "back together", db + ".restore-tmp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want it to name %q", err, want)
		}
	}
	if _, err := os.Stat(natsDir); err != nil {
		t.Fatal("NATS dir moved although the restore failed")
	}
}

// A failure after the fence says the backup is in place and fenced, where the
// previous database went, and how to finish.
func TestRestoreSQLite_NATSFailureAfterTheFenceSaysWhatStateItLeft(t *testing.T) {
	_, db, natsDir, backup, taskID := liveFarm(t)
	restore.SetRenameForTest(t, func(from, to string) error {
		if from == natsDir {
			return errors.New("injected")
		}
		return os.Rename(from, to)
	})
	_, err := restore.Run(t.Context(), opts(db, natsDir, backup))
	if err == nil {
		t.Fatal("Run with a failing NATS rename = nil, want an error")
	}
	for _, want := range []string{"injected", "restored and fenced", "restore --in-place", db + stamp, "back together"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want it to contain %q", err, want)
		}
	}
	st, err := sqlite.Open(t.Context(), db, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if task, err := st.GetTask(t.Context(), taskID); err != nil || task.Status != store.TaskStatusReady {
		t.Fatalf("task = %+v, %v; want ready (the fence ran)", task, err)
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

// requireUntouched fails t unless the live database, its -wal and the NATS
// directory are where they were and nothing was moved aside or staged.
func requireUntouched(t *testing.T, db, natsDir string) {
	t.Helper()
	if _, err := os.Stat(db); err != nil {
		t.Errorf("the live database is gone: %v", err)
	}
	if _, err := os.Stat(natsDir); err != nil {
		t.Errorf("NATS dir moved although the restore was refused: %v", err)
	}
	for _, p := range []string{db + stamp, db + "-wal" + stamp, db + ".restore-tmp"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists although the restore was refused: %v", p, err)
		}
	}
}

// A backup's -wal holds committed writes its main file lacks; restore copies
// only the main file, which still passes the integrity check, so it must refuse
// rather than lose them.
func TestRestoreSQLite_RefusesABackupWithAPendingWAL(t *testing.T) {
	_, db, natsDir, backup, _ := liveFarm(t)
	if err := os.WriteFile(backup+"-wal", []byte("frames"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := restore.Run(t.Context(), opts(db, natsDir, backup))
	if err == nil {
		t.Fatal("Run with a backup that has a non-empty -wal = nil, want a refusal")
	}
	for _, want := range []string{backup, "-wal", "would be lost", "sqi-server backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want it to contain %q", err, want)
		}
	}
	requireUntouched(t, db, natsDir)
}

// An empty -wal beside a backup (a cleanly stopped server's) holds nothing, and
// none at all is the usual case: both restore.
func TestRestoreSQLite_AcceptsABackupWithAnEmptyOrNoWAL(t *testing.T) {
	for _, tc := range []struct {
		name string
		wal  bool
	}{{"no -wal", false}, {"empty -wal", true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, db, natsDir, backup, _ := liveFarm(t)
			if tc.wal {
				if err := os.WriteFile(backup+"-wal", nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if res, err := restore.Run(t.Context(), opts(db, natsDir, backup)); err != nil || res.Fenced != 1 {
				t.Fatalf("Run = %+v, %v", res, err)
			}
		})
	}
}

// --in-place cannot tell a database's own -wal from one left by the database a
// hand-placed copy replaced, and SQLite would apply either; so it refuses a
// non-empty one, says what to do in each case, and moves nothing.
func TestRestore_InPlaceRefusesAPendingWAL(t *testing.T) {
	_, db, natsDir, _, _ := liveFarm(t)
	if err := os.WriteFile(db+"-wal", []byte("frames"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := opts(db, natsDir, "")
	o.InPlace = true
	_, err := restore.Run(t.Context(), o)
	if err == nil {
		t.Fatal("--in-place with a non-empty -wal = nil, want a refusal")
	}
	for _, want := range []string{
		"--in-place refuses", "move them aside", "sqi-server restore BACKUP",
		"sqi-server migrate up", "do not start a server on it",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want it to contain %q", err, want)
		}
	}
	// Starting a server would fold the -wal in too, but run unfenced: the
	// refusal must not offer it as a way out.
	if strings.Contains(err.Error(), "start the server") {
		t.Errorf("err = %v; it must not suggest starting the server before --in-place", err)
	}
	requireUntouched(t, db, natsDir)
	if b, err := os.ReadFile(db + "-wal"); err != nil || string(b) != "frames" {
		t.Errorf("-wal after the refusal = %q, %v; want it untouched", b, err)
	}
}

func TestRestore_InPlaceAcceptsAnEmptyWAL(t *testing.T) {
	_, db, natsDir, _, _ := liveFarm(t)
	if err := os.WriteFile(db+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	o := opts(db, natsDir, "")
	o.InPlace = true
	if res, err := restore.Run(t.Context(), o); err != nil || res.Fenced != 1 {
		t.Fatalf("--in-place with an empty -wal = %+v, %v", res, err)
	}
}

// A database copied with its own -wal while its server ran (a crash, or a
// snapshot) is refused, and the remedy the refusal names works: opening and
// closing it once (what "sqi-server migrate up" does) folds the -wal in, and
// --in-place then keeps the rows only the -wal held.
func TestRestore_InPlaceAfterACheckpointKeepsTheWALsRows(t *testing.T) {
	dir, db, natsDir, _, _ := liveFarm(t)
	st, err := sqlite.Open(t.Context(), db, sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "f-wal", Name: "only in the wal"}); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "snapshot.db")
	for _, sfx := range []string{"", "-wal"} {
		b, err := os.ReadFile(db + sfx)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(snap+sfx, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	o := opts(snap, natsDir, "")
	o.InPlace = true
	if _, err := restore.Run(t.Context(), o); err == nil || !strings.Contains(err.Error(), "--in-place refuses") {
		t.Fatalf("--in-place on a snapshot with its -wal = %v, want a refusal", err)
	}

	raw, err := sql.Open("sqlite", snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := restore.Run(t.Context(), o); err != nil || res.Fenced != 1 {
		t.Fatalf("--in-place after a checkpoint = %+v, %v", res, err)
	}
	got, err := sqlite.Open(t.Context(), snap, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if f, err := got.GetFarm(t.Context(), "f-wal"); err != nil || f.Name != "only in the wal" {
		t.Fatalf("the -wal's row after the checkpoint = %+v, %v", f, err)
	}
}

// The restored database holds password, API-key and session hashes, so it is
// readable by its owner only, like the backup it came from.
func TestRestoreSQLite_TheRestoredDatabaseIsOwnerOnly(t *testing.T) {
	_, db, natsDir, backup, _ := liveFarm(t)
	if _, err := restore.Run(t.Context(), opts(db, natsDir, backup)); err != nil {
		t.Fatal(err)
	}
	if restricted, err := fsutil.IsRestricted(db); err != nil || !restricted {
		t.Fatalf("IsRestricted(restored database) = %v, %v; want owner-only", restricted, err)
	}
}

func TestRestore_NeedsExactlyOneOfBackupAndInPlace(t *testing.T) {
	_, db, natsDir, backup, _ := liveFarm(t)
	both := opts(db, natsDir, backup)
	both.InPlace = true
	neither := opts(db, natsDir, "")
	for _, tc := range []struct {
		name string
		o    restore.Options
		want string
	}{
		{"neither", neither, "give a backup file, or --in-place, not both"},
		{"both", both, "give a backup file, or --in-place, not both"},
		{"empty SQLite path", opts("", natsDir, backup), "SQLite database path is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := restore.Run(t.Context(), tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
