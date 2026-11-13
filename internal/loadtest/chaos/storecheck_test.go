// SPDX-License-Identifier: AGPL-3.0-or-later

package chaos

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
	"github.com/uberware/sqi/internal/loadtest/scenario"
	"github.com/uberware/sqi/internal/loadtest/simworker"
	"github.com/uberware/sqi/internal/loadtest/verify"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

const (
	checkFarm   = "f1"
	checkQueue  = "q1"
	checkWorker = "w1"
)

func TestStoreCheckNamesMatchVerify(t *testing.T) {
	for _, name := range []string{verify.NameStoreIntegrity, verify.NameExecutionsRecorded} {
		if !slices.Contains(verify.Names(), name) || !slices.Contains(scenario.CheckNames, name) {
			t.Errorf("%q is not in verify.Names() %v and scenario.CheckNames %v", name, verify.Names(), scenario.CheckNames)
		}
	}
}

// testStore is a store the checks read once the test has seeded it.
type testStore struct {
	st   store.Store
	opts backend.Options
	// path is the SQLite database file, "" on PostgreSQL.
	path string
	// close closes a SQLite store, once; nil on PostgreSQL.
	close func() error
}

// stop closes a SQLite store, as the server's graceful stop does; a
// PostgreSQL store stays open (its test cleanup closes it) and is read
// through a connection of the checks' own.
func (s testStore) stop(t *testing.T) {
	t.Helper()
	if s.close == nil {
		return
	}
	if err := s.close(); err != nil {
		t.Fatalf("close the SQLite store: %v", err)
	}
}

// newSQLiteStore opens a migrated SQLite store with farm f1, queue q1 and an
// online worker w1.
func newSQLiteStore(t *testing.T) testStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := sqlite.Open(t.Context(), path, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	closeOnce := sync.OnceValue(st.Close)
	t.Cleanup(func() {
		closeOnce() //nolint:errcheck // the test is over; a close error changes nothing
	})
	ts := testStore{st: st, opts: backend.Options{Driver: backend.DriverSQLite, SQLitePath: path}, path: path, close: closeOnce}
	seedFarm(t, ts.st)
	return ts
}

func newPostgresStore(t *testing.T) testStore {
	t.Helper()
	st, dsn := storetest.PostgresAndDSN(t)
	ts := testStore{st: st, opts: backend.Options{Driver: backend.DriverPostgres, PostgresDSN: dsn}}
	seedFarm(t, ts.st)
	return ts
}

// eachBackend runs fn on SQLite, and on PostgreSQL too when it is configured.
func eachBackend(t *testing.T, fn func(t *testing.T, ts testStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, newSQLiteStore(t)) })
	if storetest.PostgresEnabled() {
		t.Run("postgres", func(t *testing.T) { fn(t, newPostgresStore(t)) })
	}
}

func seedFarm(t *testing.T, st store.Store) {
	t.Helper()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: checkFarm, Name: checkFarm}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: checkQueue, FarmID: checkFarm, Name: checkQueue}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now().UTC()
	if _, _, err := st.RegisterWorker(ctx, store.Worker{
		ID: checkWorker, FarmID: checkFarm, Hostname: checkWorker,
		Status: store.WorkerStatusOnline, CPUCount: 4, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
}

// seedSettled submits job jobID, completed, with one completed step and one
// succeeded task that never ran: settled statuses, seeded directly.
func seedSettled(t *testing.T, st store.Store, jobID, stepID, taskID string) {
	t.Helper()
	storetest.NewJob(jobID, checkFarm, checkQueue).As(store.JobStatusCompleted).
		Step(stepID, store.StepStatusCompleted).
		Task(taskID, stepID, store.TaskStatusSucceeded).
		Submit(t, st)
}

// seedRun submits job jobID with one ready task and runs it on w1 through the
// production writes, ending it as status. It returns the task's attempt.
func seedRun(t *testing.T, st store.Store, seed *storetest.JobSeed, taskID string, status store.TaskStatus) store.TaskAttempt {
	t.Helper()
	seed.Submit(t, st)
	a := storetest.Running(t, st, store.LeaseRequest{TaskID: taskID, WorkerID: checkWorker})
	storetest.Complete(t, st, a, status)
	return a
}

func readyJob(jobID, stepID, taskID string) *storetest.JobSeed {
	return storetest.NewJob(jobID, checkFarm, checkQueue).
		Step(stepID, store.StepStatusReady).
		Task(taskID, stepID, store.TaskStatusReady)
}

// ledgerOf returns a ledger holding a leased event, and a terminal one when
// outcome is not empty, for each attempt.
func ledgerOf(attempts ...ledgerEntry) *simworker.Ledger {
	l := simworker.NewLedger()
	at := time.Now()
	for _, a := range attempts {
		l.Record(simworker.Event{At: at, WorkerID: "sim-1", TaskID: a.task, AttemptID: a.id, Kind: simworker.KindLeased})
		if a.outcome != "" {
			l.Record(simworker.Event{At: at, WorkerID: "sim-1", TaskID: a.task, AttemptID: a.id, Kind: simworker.KindTerminal, Outcome: a.outcome})
		}
	}
	return l
}

type ledgerEntry struct{ id, task, outcome string }

// check runs the store checks on ts and returns them by name, failing the
// test unless they come back in order.
func check(t *testing.T, ts testStore, in StoreInput) map[string]verify.Result {
	t.Helper()
	ts.stop(t)
	if in.Options.Driver == "" && in.Options.SQLitePath == "" && in.Options.PostgresDSN == "" {
		in.Options = ts.opts
	}
	if in.FarmID == "" {
		in.FarmID = checkFarm
	}
	results := StoreChecks(t.Context(), in).Results
	var names []string
	byName := map[string]verify.Result{}
	for _, r := range results {
		names = append(names, r.Name)
		byName[r.Name] = r
	}
	if want := []string{verify.NameStoreIntegrity, verify.NameExecutionsRecorded}; !slices.Equal(names, want) {
		t.Fatalf("StoreChecks returned %v, want %v", names, want)
	}
	return byName
}

func wantOK(t *testing.T, r verify.Result) {
	t.Helper()
	if !r.OK || len(r.Evidence) != 0 {
		t.Errorf("%s = OK %v, evidence %q; want it to pass", r.Name, r.OK, r.Evidence)
	}
}

// wantFail requires r to fail with evidence naming every one of names.
func wantFail(t *testing.T, r verify.Result, names ...string) {
	t.Helper()
	if r.OK {
		t.Fatalf("%s passed, want it to fail naming %q", r.Name, names)
	}
	joined := strings.Join(r.Evidence, "\n")
	for _, n := range names {
		if !strings.Contains(joined, n) {
			t.Errorf("%s evidence %q does not name %q", r.Name, r.Evidence, n)
		}
	}
}

func TestStoreIntegrity_CleanStorePasses(t *testing.T) {
	eachBackend(t, func(t *testing.T, ts testStore) {
		seedSettled(t, ts.st, "j1", "s1", "t1")
		a := seedRun(t, ts.st, readyJob("j2", "s2", "t2"), "t2", store.TaskStatusSucceeded)
		got := check(t, ts, StoreInput{Ledger: ledgerOf(ledgerEntry{a.ID, "t2", "succeeded"})})
		wantOK(t, got[verify.NameStoreIntegrity])
		wantOK(t, got[verify.NameExecutionsRecorded])
	})
}

func TestStoreIntegrity_OpenAttemptOnTerminalTaskFails(t *testing.T) {
	ts := newSQLiteStore(t)
	seedSettled(t, ts.st, "j1", "s1", "t1")
	if _, err := storetest.InjectorFor(t, ts.st).InjectTaskAttempt(t.Context(), store.TaskAttempt{
		ID: "a-open", TaskID: "t1", WorkerID: checkWorker, AttemptNumber: 1,
		Status: store.AttemptStatusRunning, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InjectTaskAttempt: %v", err)
	}
	got := check(t, ts, StoreInput{})
	wantFail(t, got[verify.NameStoreIntegrity], "t1", "a-open")
}

func TestStoreIntegrity_InconsistentStatusesFail(t *testing.T) {
	ts := newSQLiteStore(t)
	// A completed step holding a ready task, in a completed job.
	storetest.NewJob("j1", checkFarm, checkQueue).As(store.JobStatusCompleted).
		Step("s1", store.StepStatusCompleted).
		Task("t1", "s1", store.TaskStatusReady).
		Submit(t, ts.st)
	// A completed job with a failed step.
	storetest.NewJob("j2", checkFarm, checkQueue).As(store.JobStatusCompleted).
		Step("s2", store.StepStatusFailed).
		Task("t2", "s2", store.TaskStatusFailed).
		Submit(t, ts.st)
	// A canceled job with a running step.
	storetest.NewJob("j3", checkFarm, checkQueue).As(store.JobStatusCanceled).
		Step("s3", store.StepStatusRunning).
		Task("t3", "s3", store.TaskStatusCanceled).
		Submit(t, ts.st)
	got := check(t, ts, StoreInput{})
	wantFail(t, got[verify.NameStoreIntegrity], "t1", "j2 is completed but its step s2 is failed", "j3 is canceled but its step s3 is running")
}

func TestStoreIntegrity_OtherFarmIgnored(t *testing.T) {
	ts := newSQLiteStore(t)
	seedSettled(t, ts.st, "j1", "s1", "t1")
	if _, err := ts.st.CreateFarm(t.Context(), store.Farm{ID: "f2", Name: "f2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.st.CreateQueue(t.Context(), store.Queue{ID: "q2", FarmID: "f2", Name: "q2"}); err != nil {
		t.Fatal(err)
	}
	storetest.NewJob("j9", "f2", "q2").As(store.JobStatusCompleted).
		Step("s9", store.StepStatusCompleted).
		Task("t9", "s9", store.TaskStatusReady).
		Submit(t, ts.st)
	got := check(t, ts, StoreInput{})
	wantOK(t, got[verify.NameStoreIntegrity])
}

func TestStoreIntegrity_CorruptFileFails(t *testing.T) {
	ts := newSQLiteStore(t)
	seedSettled(t, ts.st, "j1", "s1", "t1")
	ts.stop(t)
	page, pageSize := rootPage(t, ts.path, "tasks")
	data, err := os.ReadFile(ts.path)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the tasks table's root page, in the middle of the file, past
	// its 8-byte header: its cells are now garbage. (The middle page of the
	// file is a free page, which no check reads.)
	off := (page - 1) * pageSize
	for i := off + 8; i < off+pageSize; i++ {
		data[i] = 0xA5
	}
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(corrupt, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got := check(t, testStore{opts: backend.Options{Driver: backend.DriverSQLite, SQLitePath: corrupt}}, StoreInput{})
	// The row integrity_check reports names the damaged b-tree.
	wantFail(t, got[verify.NameStoreIntegrity], "integrity_check: ", "Tree ")
}

// rootPage returns the root page number of table and the database's page
// size.
func rootPage(t *testing.T, path, table string) (page, size int) {
	t.Helper()
	dsn, err := sqlite.FileURI(path, true)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(t.Context(), "SELECT rootpage FROM sqlite_schema WHERE type = 'table' AND name = ?", table).Scan(&page); err != nil {
		t.Fatalf("root page of %s: %v", table, err)
	}
	if err := db.QueryRowContext(t.Context(), "PRAGMA page_size").Scan(&size); err != nil {
		t.Fatalf("page size: %v", err)
	}
	return page, size
}

func TestStoreIntegrity_MissingDatabaseFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.db")
	got := check(t, testStore{opts: backend.Options{Driver: backend.DriverSQLite, SQLitePath: missing}}, StoreInput{})
	wantFail(t, got[verify.NameStoreIntegrity], missing)
	wantFail(t, got[verify.NameExecutionsRecorded], missing)
	if !got[verify.NameStoreIntegrity].Unmeasured || !got[verify.NameExecutionsRecorded].Unmeasured {
		t.Error("a store the checks could not open must be marked Unmeasured, so a known violation cannot excuse it")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the checks created the database they could not find: %v", err)
	}
}

func TestExecutionsRecorded_MissingAttemptFails(t *testing.T) {
	eachBackend(t, func(t *testing.T, ts testStore) {
		a := seedRun(t, ts.st, readyJob("j1", "s1", "t1"), "t1", store.TaskStatusSucceeded)
		got := check(t, ts, StoreInput{Ledger: ledgerOf(
			ledgerEntry{a.ID, "t1", "succeeded"},
			ledgerEntry{"a-missing", "t1", ""},
		)})
		r := got[verify.NameExecutionsRecorded]
		wantFail(t, r, "a-missing")
		if strings.Contains(strings.Join(r.Evidence, "\n"), a.ID) {
			t.Errorf("evidence %q names the recorded attempt %s", r.Evidence, a.ID)
		}
		wantOK(t, got[verify.NameStoreIntegrity])
	})
}

func TestExecutionsRecorded_SucceededOnWorkerFailedOnServerIsAReclaim(t *testing.T) {
	ts := newSQLiteStore(t)
	a := seedRun(t, ts.st, readyJob("j1", "s1", "t1"), "t1", store.TaskStatusFailed)
	got := check(t, ts, StoreInput{Ledger: ledgerOf(ledgerEntry{a.ID, "t1", "succeeded"})})
	wantOK(t, got[verify.NameExecutionsRecorded])
}

func TestExecutionsRecorded_FailedOnWorkerSucceededOnServerFails(t *testing.T) {
	ts := newSQLiteStore(t)
	a := seedRun(t, ts.st, readyJob("j1", "s1", "t1"), "t1", store.TaskStatusSucceeded)
	got := check(t, ts, StoreInput{Ledger: ledgerOf(ledgerEntry{a.ID, "t1", "failed"})})
	wantFail(t, got[verify.NameExecutionsRecorded], a.ID)
}

func TestExecutionsRecorded_OpenAttemptFails(t *testing.T) {
	ts := newSQLiteStore(t)
	readyJob("j1", "s1", "t1").Submit(t, ts.st)
	a := storetest.Running(t, ts.st, store.LeaseRequest{TaskID: "t1", WorkerID: checkWorker})
	got := check(t, ts, StoreInput{Ledger: ledgerOf(ledgerEntry{a.ID, "t1", ""})})
	wantFail(t, got[verify.NameExecutionsRecorded], a.ID, "running")
}

// realJob is job "job-x" whose step "step-1" has one task for frame 3.
func realJob() *storetest.JobSeed {
	seed := storetest.NewJob("j1", checkFarm, checkQueue).Step("step-1", store.StepStatusReady)
	seed.Sub.Job.Name = "job-x"
	return seed.TaskRow(store.Task{ID: "t1", StepID: "step-1", Status: store.TaskStatusReady, Parameters: map[string]string{"Frame": "3"}})
}

func mainExec(id, key string) marker.Exec {
	return marker.Exec{ID: id, Key: key, Role: marker.RoleMain, Incarnation: "0.0"}
}

func TestExecutionsRecorded_RealExecutionsExceedAttemptsFails(t *testing.T) {
	ts := newSQLiteStore(t)
	seedRun(t, ts.st, realJob(), "t1", store.TaskStatusSucceeded)
	key := scenario.TaskKey("job-x", "step-1", "3", "")
	got := check(t, ts, StoreInput{Markers: []marker.Exec{
		mainExec("e1", key), mainExec("e2", key),
		{ID: "c1", Key: key, Role: "chatty"},
	}})
	wantFail(t, got[verify.NameExecutionsRecorded], key)
}

func TestExecutionsRecorded_RealExecutionsWithinAttemptsPass(t *testing.T) {
	ts := newSQLiteStore(t)
	seedRun(t, ts.st, realJob(), "t1", store.TaskStatusSucceeded)
	key := scenario.TaskKey("job-x", "step-1", "3", "")
	// The children of one execution are not executions of the task.
	got := check(t, ts, StoreInput{Markers: []marker.Exec{
		mainExec("e1", key), {ID: "c1", Key: key, Role: "chatty"}, {ID: "c2", Key: key, Role: "silent"},
	}})
	wantOK(t, got[verify.NameExecutionsRecorded])
}

func TestExecutionsRecorded_UnknownKeyFails(t *testing.T) {
	ts := newSQLiteStore(t)
	seedRun(t, ts.st, realJob(), "t1", store.TaskStatusSucceeded)
	unknown := scenario.TaskKey("job-x", "step-1", "4", "")
	got := check(t, ts, StoreInput{Markers: []marker.Exec{mainExec("e1", unknown)}})
	wantFail(t, got[verify.NameExecutionsRecorded], unknown)
}

func TestExecutionsRecorded_SkippedMarkersAreANote(t *testing.T) {
	ts := newSQLiteStore(t)
	seedRun(t, ts.st, realJob(), "t1", store.TaskStatusSucceeded)
	key := scenario.TaskKey("job-x", "step-1", "3", "")
	got := check(t, ts, StoreInput{Markers: []marker.Exec{mainExec("e1", key)}, SkippedMarkers: 2})
	r := got[verify.NameExecutionsRecorded]
	if !r.OK {
		t.Fatalf("executions-recorded failed: %q", r.Evidence)
	}
	if len(r.Evidence) != 1 || !strings.Contains(r.Evidence[0], "2 marker files unreadable") {
		t.Errorf("evidence = %q, want one note about the 2 unreadable files", r.Evidence)
	}
	wantOK(t, got[verify.NameStoreIntegrity])
}

func TestStoreChecks_MeasureTheRework(t *testing.T) {
	ts := newSQLiteStore(t)
	a := seedRun(t, ts.st, readyJob("j1", "s1", "t1"), "t1", store.TaskStatusFailed)
	ts.stop(t)
	rep := StoreChecks(t.Context(), StoreInput{Options: ts.opts, FarmID: checkFarm, Ledger: ledgerOf(ledgerEntry{a.ID, "t1", "succeeded"})})
	if rep.ReworkErr != nil || rep.Rework == nil || rep.Rework.Attempts != 1 {
		t.Errorf("StoreChecks rework = %+v, %v; want 1 wasted attempt", rep.Rework, rep.ReworkErr)
	}
}

func TestStoreChecks_UnopenedStoreLeavesReworkUnmeasured(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	rep := StoreChecks(t.Context(), StoreInput{Options: backend.Options{Driver: backend.DriverSQLite, SQLitePath: missing}})
	if rep.Rework != nil || rep.ReworkErr == nil || !strings.Contains(rep.ReworkErr.Error(), missing) {
		t.Errorf("StoreChecks rework = %+v, %v; want unmeasured, naming %s", rep.Rework, rep.ReworkErr, missing)
	}
}
