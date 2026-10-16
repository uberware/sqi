// SPDX-License-Identifier: AGPL-3.0-or-later

package storecopy_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/schemainfo"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storecopy"
	"github.com/uberware/sqi/internal/store/storetest"
)

// batchTasks puts more tasks in one table than two insert batches hold, so
// the copy's batching (and its last, short batch) is exercised.
const batchTasks = 1201

func TestStoreCopy_CopiesEveryRowVerbatim(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)
	dst := storetest.PostgresEmptyDSN(t)
	before := fileHash(t, src)

	res, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("copied %d tables in %s", len(res.Tables), res.Elapsed)
	if len(res.Tables) != 20 {
		t.Errorf("copied %d tables, want 20", len(res.Tables))
	}
	counts := sqliteCounts(t, src)
	for _, tr := range res.Tables {
		if tr.Rows == 0 {
			t.Errorf("table %s copied 0 rows; the seed must fill every table", tr.Table)
		}
		if tr.Rows != counts[tr.Table] {
			t.Errorf("table %s: copied %d rows, source holds %d", tr.Table, tr.Rows, counts[tr.Table])
		}
	}
	assertRowsEqual(t, src, dst)

	if after := fileHash(t, src); after != before {
		t.Error("the source database file changed during the copy; store copy must never write to it")
	}
	if fi, err := os.Stat(src + "-wal"); err == nil && fi.Size() > 0 {
		t.Errorf("the copy left a %d-byte WAL beside the source", fi.Size())
	}

	// The copy is a working sqi database: a store opens on it and reads it.
	st, err := postgres.Open(t.Context(), dst, postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer st.Close()
	if f, err := st.GetFarm(t.Context(), "farm-1"); err != nil || f.Name != "Farm One" {
		t.Fatalf("GetFarm on the copy = %+v, %v", f, err)
	}
}

func TestStoreCopy_RequiresADSN(t *testing.T) {
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)
	_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src})
	if err == nil || !strings.Contains(err.Error(), "no PostgreSQL DSN") {
		t.Fatalf("Copy without a DSN = %v, want a refusal", err)
	}
}

// A missing source is refused before anything is created beside it: taking
// the source's lock would otherwise leave a stray <path>.lock behind.
func TestStoreCopy_RefusesAMissingSource(t *testing.T) {
	src := t.TempDir() + "/nothing.db"
	_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: "postgres://127.0.0.1:1/none"})
	if err == nil || !strings.Contains(err.Error(), "no SQLite database at") {
		t.Fatalf("Copy from a missing source = %v, want a refusal naming it", err)
	}
	for _, p := range []string{src, sqlite.LockPath(src), src + "-wal"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("the refusal created %s", p)
		}
	}
}

// TestStoreCopy_NeverWritesASourceWithAPendingWAL copies the database a
// crashed server leaves: committed rows that exist only in its -wal file. The
// copy must carry those rows, and must leave the database file and the WAL
// byte for byte as it found them — a read-write connection would checkpoint
// the WAL into the database file when it closed.
func TestStoreCopy_NeverWritesASourceWithAPendingWAL(t *testing.T) {
	skipWithoutPostgres(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	seedEveryTable(t, live)
	w, err := sql.Open("sqlite", live+"?_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	if _, err := w.ExecContext(t.Context(), `INSERT INTO farms (id, name, created_at, updated_at)
		VALUES ('farm-wal', 'only in the WAL', '2026-10-13T00:00:00.000000000Z', '2026-10-13T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	// Copied while the writer is open, as a crash would leave them. The name
	// carries characters a file: URI must escape.
	src := filepath.Join(dir, "crashed #1 %.db")
	copyFile(t, live, src)
	copyFile(t, live+"-wal", src+"-wal")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(src + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("the source has no pending WAL to protect: %v, %v", fi, err)
	}
	dbBefore, walBefore := fileHash(t, src), fileHash(t, src+"-wal")
	dst := storetest.PostgresEmptyDSN(t)

	if _, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst}); err != nil {
		t.Fatal(err)
	}
	if got := fileHash(t, src); got != dbBefore {
		t.Error("the copy rewrote the source database file (a WAL checkpoint): store copy must never write it")
	}
	if _, err := os.Stat(src + "-wal"); err != nil {
		t.Fatalf("the copy removed the source's WAL: %v", err)
	}
	if got := fileHash(t, src+"-wal"); got != walBefore {
		t.Error("the copy changed the source's WAL")
	}
	var name string
	if err := openPG(t, dst).QueryRowContext(t.Context(), `SELECT name FROM farms WHERE id = 'farm-wal'`).Scan(&name); err != nil {
		t.Fatalf("the row that exists only in the WAL was not copied: %v", err)
	}
	assertRowsEqual(t, src, dst)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCopy_RefusesANonEmptyTarget(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)
	dst := storetest.PostgresEmptyDSN(t)
	if _, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst}); err != nil {
		t.Fatalf("first copy: %v", err)
	}
	_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("second copy = %v, want a refusal saying the target is not empty", err)
	}
	// The first copy's rows are intact: nothing was duplicated or removed.
	assertRowsEqual(t, src, dst)
}

func TestStoreCopy_RefusesASourceBehindTheBinary(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	p, err := migrations.NewProvider(db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(t.Context(), 37); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	dst := storetest.PostgresEmptyDSN(t)

	_, err = storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if err == nil || !strings.Contains(err.Error(), "migrate up") || !strings.Contains(err.Error(), "schema 37") {
		t.Fatalf("Copy from a schema-37 source = %v, want a refusal naming \"migrate up\" and the version", err)
	}
	requireUntouched(t, dst)
}

func TestStoreCopy_RefusesASourceAheadOfTheBinary(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)
	execSQLite(t, src, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (9999, 1)`)
	dst := storetest.PostgresEmptyDSN(t)

	_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if !errors.Is(err, migrations.ErrSchemaTooNew) {
		t.Fatalf("Copy from a schema-9999 source = %v, want ErrSchemaTooNew", err)
	}
	requireUntouched(t, dst)
}

func TestStoreCopy_RefusesATargetAtAnotherVersion(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)

	t.Run("ahead", func(t *testing.T) {
		dst := storetest.PostgresDSN(t)
		execPG(t, dst, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (9999, true)`)
		_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
		if !errors.Is(err, migrations.ErrSchemaTooNew) {
			t.Fatalf("Copy into a schema-9999 target = %v, want ErrSchemaTooNew", err)
		}
	})
	t.Run("behind", func(t *testing.T) {
		dst := storetest.PostgresDSN(t)
		execPG(t, dst, `DELETE FROM goose_db_version WHERE version_id = 38`)
		_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
		if err == nil || !strings.Contains(err.Error(), "use an empty database") ||
			!strings.Contains(err.Error(), "drop and recreate") {
			t.Fatalf("Copy into a schema-37 target = %v, want a refusal saying what to do", err)
		}
	})
}

func TestStoreCopy_RefusesWhileAServerHoldsTheSource(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)
	dst := storetest.PostgresEmptyDSN(t)

	srv, err := sqlite.Open(t.Context(), src, sqlite.Options{ServerLock: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if !errors.Is(err, sqlite.ErrServerLockHeld) {
		t.Fatalf("Copy beside a server = %v, want ErrServerLockHeld", err)
	}
	requireUntouched(t, dst)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst}); err != nil {
		t.Fatalf("Copy once the server stopped: %v", err)
	}
}

func TestStoreCopy_RefusesWhileAServerHoldsTheTarget(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)
	dst := storetest.PostgresEmptyDSN(t)

	lock, err := postgres.LockDatabase(t.Context(), dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if !errors.Is(err, postgres.ErrServerLockHeld) {
		t.Fatalf("Copy beside a server on the target = %v, want ErrServerLockHeld", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	requireUntouched(t, dst)
	// The refusal released the source's lock: a server can start on it.
	srv, err := sqlite.Open(t.Context(), src, sqlite.Options{ServerLock: true})
	if err != nil {
		t.Fatalf("the source lock was not released: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
}

// A table sqi does not own is refused by name, before anything is written;
// the copy would otherwise leave it, and its rows, beside sqi's.
func TestStoreCopy_RefusesAForeignTableInTheTarget(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)

	t.Run("empty database", func(t *testing.T) {
		dst := storetest.PostgresEmptyDSN(t)
		execPG(t, dst, `CREATE TABLE notes (id int primary key)`)
		_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
		if err == nil || !strings.Contains(err.Error(), "notes") {
			t.Fatalf("Copy with a foreign table = %v, want a refusal naming it", err)
		}
		// Refused before the migration: no sqi table was created beside it.
		if got := pgTables(t, dst); !slices.Equal(got, []string{"notes"}) {
			t.Fatalf("tables after the refusal = %v, want only notes", got)
		}
	})
	t.Run("migrated database", func(t *testing.T) {
		dst := storetest.PostgresDSN(t)
		// Empty, so only the shape check — not the emptiness check — can see it.
		execPG(t, dst, `CREATE TABLE notes (id int primary key)`)
		_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
		if err == nil || !strings.Contains(err.Error(), "table notes exists only in the PostgreSQL database") {
			t.Fatalf("Copy with a foreign table = %v, want a refusal naming it", err)
		}
		if n := pgCount(t, dst, "farms"); n != 0 {
			t.Fatalf("farms holds %d rows after the refusal, want 0", n)
		}
	})
	t.Run("foreign column", func(t *testing.T) {
		dst := storetest.PostgresDSN(t)
		execPG(t, dst, `ALTER TABLE farms ADD COLUMN notes text`)
		_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
		if err == nil || !strings.Contains(err.Error(), "farms.notes") {
			t.Fatalf("Copy with a foreign column = %v, want a refusal naming it", err)
		}
	})
}

func TestStoreCopy_RefusesANonBooleanInABooleanColumn(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)
	execSQLite(t, src, `UPDATE queues SET paused = 2 WHERE id = 'queue-1'`)
	dst := storetest.PostgresEmptyDSN(t)

	_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if err == nil {
		t.Fatal("Copy of paused = 2 succeeded")
	}
	for _, want := range []string{"queues", "queue-1", "paused", "no sqi rows were committed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	requireNoRows(t, dst)
}

// A value PostgreSQL refuses aborts the copy naming the table, the row's
// primary key and the column, and leaves the target with no rows.
func TestStoreCopy_NamesTheRowAndColumnPostgreSQLRefuses(t *testing.T) {
	skipWithoutPostgres(t)
	src := t.TempDir() + "/sqi.db"
	seedEveryTable(t, src)
	// Not the fixed-width text the ts domain's CHECK enforces.
	execSQLite(t, src, `UPDATE tasks SET created_at = '2026-10-12 00:00:00' WHERE id = 'task-0700'`)
	dst := storetest.PostgresEmptyDSN(t)

	_, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if err == nil {
		t.Fatal("Copy of a malformed timestamp succeeded")
	}
	for _, want := range []string{"tasks", "task-0700", "created_at", "no sqi rows were committed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	requireNoRows(t, dst)
}

// TestStoreCopy_MeasureLargeCopy times a copy of a farm with
// SQI_STORECOPY_MEASURE_TASKS tasks in one job. It asserts nothing beyond
// success and logs the seed and copy times.
func TestStoreCopy_MeasureLargeCopy(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("SQI_STORECOPY_MEASURE_TASKS"))
	if err != nil || n <= 0 {
		t.Skip("set SQI_STORECOPY_MEASURE_TASKS to the task count to measure")
	}
	src := t.TempDir() + "/sqi.db"
	st, err := sqlite.Open(t.Context(), src, sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	seedFarmQueue(t, st)
	seed := storetest.NewJob("job-big", "farm-1", "queue-1").As(store.JobStatusRunning).Step("step-big", store.StepStatusReady)
	params := map[string]string{"Frame": "0"}
	for i := range n {
		seed.TaskRow(store.Task{ID: fmt.Sprintf("big-%07d", i), StepID: "step-big", Status: store.TaskStatusReady, Parameters: params})
	}
	start := time.Now()
	seed.Submit(t, st)
	t.Logf("seeded %d tasks in %s", n, time.Since(start))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(src); err == nil {
		t.Logf("source database: %d bytes", fi.Size())
	}
	dst := storetest.PostgresEmptyDSN(t)
	res, err := storecopy.Copy(t.Context(), storecopy.Options{SQLitePath: src, PostgresDSN: dst})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, tr := range res.Tables {
		total += tr.Rows
	}
	t.Logf("store copy: %d rows (%d tasks) copied and verified in %s", total, n, res.Elapsed)
}

// seedEveryTable creates a migrated SQLite database at path and fills every
// table through production store writes, then fails unless every table
// schemainfo lists holds a row, so a table without a seed fails here rather
// than going uncopied by the tests.
func seedEveryTable(t *testing.T, path string) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, path, sqlite.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	later := now.Add(24 * time.Hour)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}

	seedFarmQueue(t, st)
	runAs := "render"
	_, err = st.CreateQueue(ctx, store.Queue{
		ID: "queue-2", FarmID: "farm-1", Name: "paused", Paused: true, Priority: 7, RunAsUser: &runAs,
	})
	must("paused queue", err)

	admin, err := st.CreateUser(ctx, store.User{
		ID: "user-1", Username: "Admin", DisplayName: "Ådmin Üser", PasswordHash: "$argon2id$x", Role: "admin",
		AuthSource: store.AuthSourceLocal,
	})
	must("user", err)
	_, err = st.CreateUser(ctx, store.User{
		ID: "user-2", Username: "gone", PasswordHash: "", Role: "user", AuthSource: "oidc", ExternalID: "sub-2", Disabled: true,
	})
	must("disabled user", err)
	_, err = st.CreateAPIKey(ctx, store.APIKey{
		ID: "key-1", UserID: admin.ID, Name: "ci", Prefix: "sqi_ab", TokenHash: "hash-key-1", ExpiresAt: &later,
	})
	must("api key", err)
	_, err = st.CreateSession(ctx, store.Session{ID: "session-1", TokenHash: "hash-session-1", UserID: admin.ID, ExpiresAt: later})
	must("session", err)
	must("audit entry", st.AppendAuditEntry(ctx, store.AuditEntry{
		ID: "audit-1", EntityType: "job", EntityID: "job-main", Action: "submitted", Actor: "Admin",
		Details: map[string]any{"note": "quote \" and newline \n and tab \t", "n": 3},
	}))
	_, err = st.CreateComputeLocation(ctx, store.ComputeLocation{ID: "cl-1", Name: "onprem", Description: "rack 3"})
	must("compute location", err)
	_, err = st.CreateStorageLocation(ctx, store.StorageLocation{
		ID: "sl-1", Name: "projects", Type: store.StorageLocationTypeFilesystem, Roots: map[string]string{"onprem": `\\nas\projects`},
	})
	must("storage location", err)
	_, err = st.CreateProduct(ctx, store.Product{
		ID: "product-1", Name: "studio/render", Title: "Render", Source: store.SourceCustom,
		Template: "specificationVersion: jobtemplate-2023-09\nname: x\n", Format: store.TemplateFormatYAML, Readme: "# Render\n",
	})
	must("product", err)
	pool, err := st.CreateUsagePool(ctx, store.UsagePool{ID: "pool-1", Name: "nuke", ServerHint: "10.0.0.50:5053", MaxConcurrent: 4})
	must("usage pool", err)
	_, _, err = st.RegisterWorker(ctx, store.Worker{
		ID: "worker-1", FarmID: "farm-1", QueueID: "queue-1", Name: "node 1", Hostname: "node1", IPAddress: "10.0.0.7",
		ComputeLocation: "onprem", OS: "linux", OSVersion: "6.1", Arch: "amd64", Version: "0.3.0", InstanceID: "i-1",
		CPUCount: 16, RAMMb: 65536, Tags: map[string]string{"gpu": "yes"}, Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
	})
	must("worker", err)
	_, _, err = st.RegisterWorker(ctx, store.Worker{ID: "worker-2", Hostname: "node2", Status: store.WorkerStatusOffline})
	must("second worker", err)
	_, err = st.SetWorkerDisabled(ctx, "worker-2", true)
	must("disable worker", err)
	_, err = st.CreateWorkerCredential(ctx, store.WorkerCredential{ID: "cred-1", WorkerID: "worker-1", PublicKey: "ssh-ed25519 AAAA", Name: "node1"})
	must("worker credential", err)
	_, err = st.CreateWorkerJoinToken(ctx, store.WorkerJoinToken{
		ID: "join-1", TokenHash: "hash-join-1", Prefix: "sqj_ab", Name: "rack 3", ExpiresAt: later, CreatedBy: "Admin",
	})
	must("join token", err)

	// An upstream job, complete, and a running job that depends on it.
	storetest.NewJob("job-up", "farm-1", "queue-1").As(store.JobStatusCompleted).
		Step("step-up", store.StepStatusCompleted).Task("task-up", "step-up", store.TaskStatusSucceeded).Submit(t, st)
	job := storetest.NewJob("job-main", "farm-1", "queue-1").As(store.JobStatusRunning)
	job.Sub.DependsOn = []string{"job-up"}
	job.Sub.Job.Owner = "Admin"
	job.Sub.Job.Parameters = map[string]string{"Scene": "/proj/shot 010/ünïcode.ma"}
	job.Step("step-a", store.StepStatusRunning)
	job.StepRow(store.Step{ID: "step-b", Status: store.StepStatusPending, DependsOn: []string{"step-a"}, ComputeLocation: "onprem"})
	cores := 4
	job.TaskRow(store.Task{ID: "task-done", StepID: "step-a", Status: store.TaskStatusReady, RequiredCores: &cores})
	job.Task("task-running", "step-a", store.TaskStatusReady)
	for i := range batchTasks {
		job.TaskRow(store.Task{
			ID: fmt.Sprintf("task-%04d", i), StepID: "step-b", Status: store.TaskStatusPending,
			Parameters: map[string]string{"Frame": strconv.Itoa(i)},
		})
	}
	job.Submit(t, st)

	claim := func() []store.UsagePoolClaim {
		return []store.UsagePoolClaim{{ClaimID: uuid.NewString(), PoolID: pool.ID, PoolName: pool.Name}}
	}
	done := storetest.Running(t, st, store.LeaseRequest{TaskID: "task-done", WorkerID: "worker-1", Claims: claim()})
	storetest.Complete(t, st, done, store.TaskStatusSucceeded)
	running := storetest.Running(t, st, store.LeaseRequest{TaskID: "task-running", WorkerID: "worker-1", Claims: claim()})
	for i, data := range []string{"frame 1 ✓\n", "warning:\ttab\r\n"} {
		stream := store.LogStreamStdout
		if i == 1 {
			stream = store.LogStreamStderr
		}
		_, err := st.CreateTaskLog(ctx, store.TaskLog{
			ID: uuid.NewString(), TaskID: running.TaskID, AttemptID: running.ID, SeqNum: int64(i + 1), NATSSeq: int64(100 + i),
			At: now, ReceivedAt: now, Stream: stream, Data: data,
		})
		must("task log", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	for tbl, n := range sqliteCounts(t, path) {
		if n == 0 {
			t.Fatalf("seedEveryTable left %s empty; seed it so the copy tests cover it", tbl)
		}
	}
}

func seedFarmQueue(t *testing.T, st store.Store) {
	t.Helper()
	maxAttempts := 3
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "Farm One", MaxAttempts: &maxAttempts}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(t.Context(), store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "default"}); err != nil {
		t.Fatal(err)
	}
}

// sqliteCounts returns the row count of every table schemainfo lists.
func sqliteCounts(t *testing.T, path string) map[string]int64 {
	t.Helper()
	db := openSQLite(t, path)
	info, err := schemainfo.SQLite(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int64, len(info))
	for tbl := range info {
		var n int64
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM "`+tbl+`"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[tbl] = n
	}
	return out
}

// assertRowsEqual compares every table of the SQLite database at src with
// the PostgreSQL database dst, row by row and value by value, without
// storecopy's digest or schemainfo: its own catalog queries, SELECTs ordered by
// primary key, SQLite text read through CAST(... AS TEXT) (the driver would
// decode a TIMESTAMP column into a time.Time), and SQLite's 0/1 compared with
// PostgreSQL's booleans.
func assertRowsEqual(t *testing.T, src, dst string) {
	t.Helper()
	lite, pg := openSQLite(t, src), openPG(t, dst)
	var tables []string
	for _, row := range selectAll(t, lite,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'goose_db_version' ORDER BY name`) {
		tables = append(tables, fmt.Sprint(row[0]))
	}
	if got := pgTables(t, dst); !slices.Equal(got, tables) {
		t.Fatalf("PostgreSQL tables %v, SQLite tables %v", got, tables)
	}
	for _, tbl := range tables {
		cols, pk := liteColumns(t, lite, tbl)
		types := pgTypes(t, pg, tbl)
		var liteSel, pgSel []string
		for _, c := range cols {
			q := `"` + c + `"`
			pgSel = append(pgSel, q)
			if types[c] == "text" {
				q = `CAST(` + q + ` AS TEXT)`
			}
			liteSel = append(liteSel, q)
		}
		order := `"` + strings.Join(pk, `", "`) + `"`
		a := selectAll(t, lite, `SELECT `+strings.Join(liteSel, ", ")+` FROM "`+tbl+`" ORDER BY `+order)
		b := selectAll(t, pg, `SELECT `+strings.Join(pgSel, ", ")+` FROM "`+tbl+`" ORDER BY `+order)
		if len(a) != len(b) {
			t.Errorf("%s: SQLite %d rows, PostgreSQL %d", tbl, len(a), len(b))
			continue
		}
		for i := range a {
			for j, c := range cols {
				if !sameValue(a[i][j], b[i][j], types[c]) {
					t.Errorf("%s row %d (%v) column %s: SQLite %#v, PostgreSQL %#v", tbl, i, a[i][0], c, a[i][j], b[i][j])
				}
			}
		}
	}
}

func sameValue(lite, pg any, pgType string) bool {
	if lite == nil || pg == nil {
		return lite == nil && pg == nil
	}
	switch pgType {
	case "boolean":
		n, ok := lite.(int64)
		b, okB := pg.(bool)
		return ok && okB && (n == 0 || n == 1) && (n == 1) == b
	case "bigint":
		n, ok := lite.(int64)
		m, okM := pg.(int64)
		return ok && okM && n == m
	default:
		s, ok := lite.(string)
		u, okU := pg.(string)
		return ok && okU && s == u
	}
}

func liteColumns(t *testing.T, db *sql.DB, tbl string) (cols, pk []string) {
	t.Helper()
	keyAt := map[int64]string{}
	for _, row := range selectAll(t, db, `SELECT name, pk FROM pragma_table_info('`+tbl+`') ORDER BY cid`) {
		c := fmt.Sprint(row[0])
		k, err := strconv.ParseInt(fmt.Sprint(row[1]), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
		if k > 0 {
			keyAt[k] = c
		}
	}
	for i := int64(1); i <= int64(len(keyAt)); i++ {
		pk = append(pk, keyAt[i])
	}
	if len(pk) == 0 {
		t.Fatalf("%s has no primary key", tbl)
	}
	return cols, pk
}

func pgTypes(t *testing.T, db *sql.DB, tbl string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, row := range selectAll(t, db, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = '`+tbl+`'`) {
		c := fmt.Sprint(row[0])
		ty := fmt.Sprint(row[1])
		out[c] = ty
	}
	return out
}

func selectAll(t *testing.T, db *sql.DB, q string) [][]any {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// pgTables lists the tables in the PostgreSQL database's public schema, but
// goose's, sorted.
func pgTables(t *testing.T, dsn string) []string {
	t.Helper()
	var out []string
	for _, row := range selectAll(t, openPG(t, dsn), `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name != 'goose_db_version' ORDER BY table_name`) {
		n := fmt.Sprint(row[0])
		out = append(out, n)
	}
	return out
}

func pgCount(t *testing.T, dsn, tbl string) int64 {
	t.Helper()
	var n int64
	if err := openPG(t, dsn).QueryRowContext(t.Context(), `SELECT count(*) FROM "`+tbl+`"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requireUntouched fails unless the database dsn names holds no table at all:
// the copy refused before it migrated anything.
func requireUntouched(t *testing.T, dsn string) {
	t.Helper()
	var present bool
	if err := openPG(t, dsn).QueryRowContext(t.Context(), `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if got := pgTables(t, dsn); len(got) != 0 || present {
		t.Fatalf("the target was written although the copy refused: tables %v, goose table %v", got, present)
	}
}

// requireNoRows fails unless every table in the target is empty: a failed
// copy commits nothing.
func requireNoRows(t *testing.T, dsn string) {
	t.Helper()
	for _, tbl := range pgTables(t, dsn) {
		if n := pgCount(t, dsn, tbl); n != 0 {
			t.Errorf("%s holds %d rows after a failed copy, want 0", tbl, n)
		}
	}
}

func openPG(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openSQLite opens the database at path read-only (a mode=ro URI), so that
// reading it in a test cannot checkpoint its WAL into it either.
func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn, err := sqlite.FileURI(path, true)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execPG(t *testing.T, dsn, q string) {
	t.Helper()
	if _, err := openPG(t, dsn).ExecContext(t.Context(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func execSQLite(t *testing.T, path, q string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// skipWithoutPostgres skips before the slow seed when no PostgreSQL is
// configured. storetest.PostgresEnabled is true under
// SQI_TEST_REQUIRE_POSTGRES=1, so there a missing server still fails the test.
func skipWithoutPostgres(t *testing.T) {
	t.Helper()
	if !storetest.PostgresEnabled() {
		t.Skip("PostgreSQL not configured: set SQI_TEST_POSTGRES_DSN, or run make test-postgres")
	}
}
