// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package integration

// restore_test.go — the backup, restore and rollback runbooks of
// docs/operations.md, executed against the real server code with a real
// sqi-worker subprocess (H5 design §8.4).
//
// The worker is started once and outlives every server restart in a test: it
// reconnects to the next server on the same NATS address under its stable
// worker ID, exactly as a farm's workers ride out a server restore. Whatever
// it was running when the server stopped becomes a ghost run whose late
// reports reach a database that no longer expects them — which is the case the
// restore fence and the discard of superseded reports exist for.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/restore"
	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/store/backend"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

// sleepJob is a template whose tasks each sleep long enough that a backup
// taken while it runs catches tasks running. Each task prints "done N" once;
// printf's format keeps that text out of the command line itself, so a log
// that echoes the command cannot be mistaken for the output. POSIX only
// (startRealWorker skips Windows), like the other real-worker tests.
const sleepJob = `specificationVersion: jobtemplate-2023-09
name: restore-e2e
steps:
  - name: work
    parameterSpace:
      taskParameterDefinitions:
        - name: N
          type: INT
          range: "1-6"
    script:
      actions:
        onRun:
          command: /bin/sh
          args: ['-c', 'sleep 3; printf "done %s\n" {{Task.Param.N}}']
`

// sleepJobTasks is the number of tasks sleepJob expands to.
const sleepJobTasks = 6

// logsConsumer is the durable name of the server's log-ingest consumer
// (internal/bus, taskLogsConsumerName).
const logsConsumer = "sqi-task-logs-srv"

// TestBackupRestore_FarmResumes is the restore runbook, executed: back up
// while tasks run, let the farm move on, stop, restore, start; every task of
// every job finishes, none is stranded, and retention deletes none of them.
//
// Reconciliation is off, so the restore fence alone must recover the attempts
// the backup caught running: with the fence disabled, job A's tasks stay
// running forever under a worker that is alive and no longer runs them.
func TestBackupRestore_FarmResumes(t *testing.T) {
	ts := startServerWith(t, func(c *server.Config) { c.Scheduler.ReconcileGrace = 0 })
	requireBackupTools(t, ts.Store)
	farmID, queueID := seedFarmAndQueue(t, ts)
	startRestartableWorker(t, ts, farmID, queueID)

	jobA := submitJobCustomYAML(t, ts, farmID, queueID, sleepJob)
	pollTaskStatus(t, ts, firstTaskID(t, ts, jobA), []string{"running"}, 30*time.Second)

	backup := filepath.Join(t.TempDir(), "backup")
	if err := backend.Backup(t.Context(), ts.Store, backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// The farm moves on after the backup: job A finishes, job B is submitted
	// and is running when the server stops, so its ghost runs report into a
	// database that has never heard of it.
	waitJobCompleted(t, ts, jobA, 2*time.Minute)
	jobB := submitJobCustomYAML(t, ts, farmID, queueID, sleepJob)
	pollTaskStatus(t, ts, firstTaskID(t, ts, jobB), []string{"running"}, 30*time.Second)

	ts.Stop(t)
	if ts.Store.Driver == backend.DriverPostgres {
		ts.Store.PostgresDSN = storetest.PostgresEmptyDSN(t) // restore into a fresh database
		ts.cfg.PostgresDSN = ts.Store.PostgresDSN
	}
	res, err := restore.Run(t.Context(), restore.Options{
		Driver: ts.Store.Driver, SQLitePath: ts.Store.SQLitePath, PostgresDSN: ts.Store.PostgresDSN,
		NATSDataDir: ts.NATSDir, Backup: backup,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Fenced == 0 {
		t.Fatal("the backup caught running tasks, so the fence must have reclaimed some")
	}
	if res.NATSMovedTo == "" {
		t.Fatalf("restore left the NATS directory %s in place; the runbook moves it aside", ts.NATSDir)
	}
	t.Logf("restore fenced %d attempts and moved the NATS directory to %s", res.Fenced, res.NATSMovedTo)
	ts = restartServer(t, ts)

	// Job A is back as the backup recorded it (part-done) and runs to the end;
	// job B never existed in the backup.
	waitJobCompleted(t, ts, jobA, 3*time.Minute)
	if code := getStatusCode(t, ts, "/api/v1/jobs/"+jobB); code != http.StatusNotFound {
		t.Fatalf("job B after restore: HTTP %d, want 404 (submitted after the backup)", code)
	}
	assertNoInFlightTasks(t, ts)
	// Each fenced attempt's task ran again under a new attempt.
	if got, want := attemptCount(t, ts, jobA), sleepJobTasks+res.Fenced; got < want {
		t.Fatalf("job A has %d attempts, want at least %d (%d tasks, %d fenced and run again)",
			got, want, sleepJobTasks, res.Fenced)
	}
}

// TestRestore_MixedAgeNATSDir restores the database by hand — no restore
// command, so no fence and no NATS quarantine — beside a NATS directory of a
// different age, and starts the server on the pair.
func TestRestore_MixedAgeNATSDir(t *testing.T) {
	reconcile := func(c *server.Config) {
		c.Scheduler.ReconcileGrace = 40 * time.Second
		c.Scheduler.WorkerTimeout = 30 * time.Second
	}

	// older: the NATS directory predates the database. Its log stream holds
	// job 1's chunks, which the database already stored; replayed, each must be
	// stored once (task_logs is unique on attempt and worker sequence).
	t.Run("older", func(t *testing.T) {
		ts := startServerWith(t, reconcile)
		requireBackupTools(t, ts.Store)
		farmID, queueID := seedFarmAndQueue(t, ts)
		startRestartableWorker(t, ts, farmID, queueID)

		job1 := submitJobCustomYAML(t, ts, farmID, queueID, sleepJob)
		waitJobCompleted(t, ts, job1, 2*time.Minute)
		waitJobLogs(t, ts, job1)
		ts.Stop(t)
		oldNATS := filepath.Join(t.TempDir(), "nats-old")
		copyDir(t, ts.NATSDir, oldNATS)

		ts = restartServer(t, ts)
		job2 := submitJobCustomYAML(t, ts, farmID, queueID, sleepJob)
		waitJobCompleted(t, ts, job2, 2*time.Minute)
		waitJobLogs(t, ts, job2)
		backup := filepath.Join(t.TempDir(), "backup")
		if err := backend.Backup(t.Context(), ts.Store, backup); err != nil {
			t.Fatalf("backup: %v", err)
		}
		ts.Stop(t)

		if err := os.RemoveAll(ts.NATSDir); err != nil {
			t.Fatal(err)
		}
		copyDir(t, oldNATS, ts.NATSDir)
		// Whether the copy alone replays anything depends on how much of the
		// log consumer's ack state JetStream had written to disk when the
		// server stopped (measured on Linux, it replayed all of job 1's chunks
		// without this too). Dropping that state makes the replay certain: it models a
		// directory whose consumer state is older than its stream, and the
		// server recreates the consumer and delivers every chunk again.
		dropConsumerState(t, ts.NATSDir, bus.StreamLogs, logsConsumer)
		restoreByHand(t, ts, backup)
		ts = restartServer(t, ts)

		if replayed := waitLogReplayDrained(t, ts); replayed == 0 {
			t.Fatal("the log consumer delivered nothing after the restart; the replay this subtest exists to see did not happen")
		} else {
			t.Logf("log consumer replayed %d chunks", replayed)
		}
		for _, job := range []string{job1, job2} {
			if got := jobStatus(t, ts, job); got != "completed" {
				t.Errorf("job %s after restart: %q, want completed", job, got)
			}
			logs := jobLogText(t, ts, job)
			for n := 1; n <= sleepJobTasks; n++ {
				line := fmt.Sprintf("done %d", n)
				if c := strings.Count(logs, line); c != 1 {
					t.Errorf("job %s: %q appears %d times in its logs, want exactly once", job, line, c)
				}
			}
		}
		assertNoInFlightTasks(t, ts)
	})

	// newer: the NATS directory postdates the database. The terminal reports
	// of the tasks the backup caught running were consumed (and, from a
	// work-queue stream, deleted) before the restore, so nothing will ever
	// report them again; reconciliation reclaims them because the worker's
	// heartbeats no longer list their attempts.
	t.Run("newer", func(t *testing.T) {
		ts := startServerWith(t, reconcile)
		requireBackupTools(t, ts.Store)
		farmID, queueID := seedFarmAndQueue(t, ts)
		startRestartableWorker(t, ts, farmID, queueID)

		job := submitJobCustomYAML(t, ts, farmID, queueID, sleepJob)
		pollTaskStatus(t, ts, firstTaskID(t, ts, job), []string{"running"}, 30*time.Second)
		backup := filepath.Join(t.TempDir(), "backup")
		if err := backend.Backup(t.Context(), ts.Store, backup); err != nil {
			t.Fatalf("backup: %v", err)
		}
		waitJobCompleted(t, ts, job, 2*time.Minute)
		ts.Stop(t)

		restoreByHand(t, ts, backup) // the current, newer NATS directory stays
		stale := openAttemptCount(t, ts.Store)
		if stale == 0 {
			t.Fatal("the restored database has no open attempt; the backup caught no running task to reconcile")
		}
		ts = restartServer(t, ts)

		grace := ts.cfg.Scheduler.ReconcileGrace
		waitJobCompleted(t, ts, job, grace+60*time.Second)
		assertNoInFlightTasks(t, ts)
		// The worker stayed online throughout, so the offline sweep never
		// reclaimed anything: reconciliation did.
		if got := metricValue(t, ts, "sqi_scheduler_tasks_reconciled_total"); got < 1 {
			t.Fatalf("sqi_scheduler_tasks_reconciled_total = %v after recovering %d stale attempts, want at least 1", got, stale)
		}
	})
}

// TestUpgrade_OlderBinaryRefused is the rollback runbook: a database a newer
// sqi-server migrated is refused at start, and restoring the backup taken
// before the upgrade brings the older server back up on the old data. It runs
// a job with the mock worker, so it needs no POSIX shell and runs on Windows.
func TestUpgrade_OlderBinaryRefused(t *testing.T) {
	ts := startServer(t)
	requireBackupTools(t, ts.Store)
	farmID, queueID := seedFarmAndQueue(t, ts)
	jobID := runMockJob(t, ts, farmID, queueID)

	backup := filepath.Join(t.TempDir(), "backup")
	if err := backend.Backup(t.Context(), ts.Store, backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	ts.Stop(t)

	// A newer binary's migration.
	driver := backend.Name(ts.Store.Driver)
	maxV, err := migrations.MaxVersion(driver)
	if err != nil {
		t.Fatal(err)
	}
	addGooseVersion(t, ts.Store, maxV+1)

	if _, err := tryBootServer(t, ts.cfg, ts.Store); !errors.Is(err, migrations.ErrSchemaTooNew) {
		t.Fatalf("start on a schema-%d database = %v, want ErrSchemaTooNew", maxV+1, err)
	} else {
		var tooNew *migrations.SchemaTooNewError
		if !errors.As(err, &tooNew) || tooNew.DBVersion != maxV+1 || tooNew.BinaryVersion != maxV {
			t.Fatalf("start error = %v, want one naming database version %d and binary version %d", err, maxV+1, maxV)
		}
	}

	// Roll back: restore the pre-upgrade backup with this (older) binary.
	if ts.Store.Driver == backend.DriverPostgres {
		ts.Store.PostgresDSN = storetest.PostgresEmptyDSN(t)
		ts.cfg.PostgresDSN = ts.Store.PostgresDSN
	}
	if _, err := restore.Run(t.Context(), restore.Options{
		Driver: ts.Store.Driver, SQLitePath: ts.Store.SQLitePath, PostgresDSN: ts.Store.PostgresDSN,
		NATSDataDir: ts.NATSDir, Backup: backup,
	}); err != nil {
		t.Fatalf("restore the pre-upgrade backup: %v", err)
	}
	ts = restartServer(t, ts)
	if got := jobStatus(t, ts, jobID); got != "completed" {
		t.Fatalf("job after the rollback: %q, want completed", got)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// startRestartableWorker is startRealWorker with a short NATS reconnect wait.
// The worker's reconnect delay doubles from that base on every failed attempt,
// so with the default 2 s base an outage of a few seconds could leave it
// waiting tens of seconds before it rejoins the restarted server.
func startRestartableWorker(t *testing.T, ts *testServer, farmID, queueID string) string {
	t.Helper()
	return startRealWorkerWithOptions(t, ts, farmID, queueID, nil, []string{"SQI_WORKER_NATS_RECONNECT_WAIT=200ms"})
}

// runMockJob runs minimalJobYAML to completion with an in-process mock worker
// and returns the job's ID.
func runMockJob(t *testing.T, ts *testServer, farmID, queueID string) string {
	t.Helper()
	const workerID = "worker-upgrade-001"
	w := newMockWorker(t, "nats://"+ts.NATSAddr, workerID, farmID, queueID)
	w.register()
	w.startHeartbeat(5 * time.Second)
	pollWorkerOnline(t, ts, workerID, 10*time.Second)

	jobID := submitJob(t, ts, farmID, queueID)
	assign := w.pullAssignment(15 * time.Second)
	w.publishStatus(assign, "running", nil)
	exitCode := 0
	w.publishStatus(assign, "succeeded", &exitCode)
	waitJobCompleted(t, ts, jobID, 20*time.Second)
	return jobID
}

// requireBackupTools skips (or, under SQI_TEST_REQUIRE_POSTGRES=1, fails) a
// PostgreSQL run without the client tools backup and restore shell out to.
func requireBackupTools(t *testing.T, so backend.Options) {
	t.Helper()
	if so.Driver == backend.DriverPostgres {
		storetest.RequirePostgresTool(t, "pg_dump")
		storetest.RequirePostgresTool(t, "pg_restore")
	}
}

// restoreByHand puts backup in place of ts's database the way an operator
// without `sqi-server restore` would: no fence, and the NATS directory left as
// it is. SQLite: the backup is copied over the database file. PostgreSQL: it
// is restored into a fresh empty database, which ts is pointed at.
func restoreByHand(t *testing.T, ts *testServer, backup string) {
	t.Helper()
	if ts.Store.Driver == backend.DriverPostgres {
		dsn := storetest.PostgresEmptyDSN(t)
		if err := postgres.Restore(t.Context(), dsn, backup, nil); err != nil {
			t.Fatalf("pg_restore the backup: %v", err)
		}
		ts.Store.PostgresDSN = dsn
		ts.cfg.PostgresDSN = dsn
		return
	}
	// The server stopped cleanly, so its WAL is checkpointed; a stale -wal or
	// -shm beside the copied file would be applied to the wrong database.
	for _, p := range []string{ts.Store.SQLitePath + "-wal", ts.Store.SQLitePath + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	copyFile(t, backup, ts.Store.SQLitePath)
}

// addGooseVersion records version as applied in so's goose table, as a newer
// sqi-server's migration would.
func addGooseVersion(t *testing.T, so backend.Options, version int64) {
	t.Helper()
	db, q := openSQL(t, so), `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`
	if so.Driver == backend.DriverPostgres {
		q = `INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`
	}
	_, err := db.ExecContext(t.Context(), q, version)
	// Closed now, not at cleanup: Windows cannot move an open file aside, and
	// the rollback restore moves this database aside.
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("record schema version %d: %v", version, err)
	}
}

// openSQL opens a plain database/sql handle on so's database (the drivers are
// registered by the store packages). The caller closes it.
func openSQL(t *testing.T, so backend.Options) *sql.DB {
	t.Helper()
	driver, dsn := "sqlite", so.SQLitePath
	if so.Driver == backend.DriverPostgres {
		driver, dsn = "pgx", so.PostgresDSN
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open %s database: %v", driver, err)
	}
	return db
}

// openAttemptCount reports how many attempts so's database records as open.
func openAttemptCount(t *testing.T, so backend.Options) int {
	t.Helper()
	o := so
	o.MaxConns = 2
	st, err := backend.Open(t.Context(), o)
	if err != nil {
		t.Fatalf("open the restored store: %v", err)
	}
	defer st.Close()
	open, err := st.ListOpenAttempts(t.Context(), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("list open attempts: %v", err)
	}
	return len(open)
}

// waitJobCompleted waits for jobID to reach a terminal status and fails the
// test unless it is completed.
func waitJobCompleted(t *testing.T, ts *testServer, jobID string, timeout time.Duration) {
	t.Helper()
	if got := pollJobStatus(t, ts, jobID, []string{"completed", "failed", "canceled"}, timeout); got != "completed" {
		t.Fatalf("job %s finished %q, want completed", jobID, got)
	}
}

// jobStatus returns jobID's current status.
func jobStatus(t *testing.T, ts *testServer, jobID string) string {
	t.Helper()
	var resp jobResp
	mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID), nil, "", http.StatusOK, &resp)
	return resp.Status
}

// getStatusCode returns the HTTP status of GET path.
func getStatusCode(t *testing.T, ts *testServer, path string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, apiURL(ts, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(ts).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining for connection reuse; only the status matters
	return resp.StatusCode
}

// taskSummary is the subset of a task the runbook assertions read.
type taskSummary struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// jobTasks returns every task of jobID.
func jobTasks(t *testing.T, ts *testServer, jobID string) []taskSummary {
	t.Helper()
	var resp struct {
		Items []taskSummary `json:"items"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/jobs/"+jobID+"/tasks?limit=1000"),
		nil, "", http.StatusOK, &resp)
	return resp.Items
}

// attemptSummary is the subset of a task attempt the runbook assertions read.
type attemptSummary struct {
	Status  string     `json:"status"`
	EndedAt *time.Time `json:"ended_at"`
}

// taskAttempts returns every attempt of taskID.
func taskAttempts(t *testing.T, ts *testServer, taskID string) []attemptSummary {
	t.Helper()
	var resp struct {
		Items []attemptSummary `json:"items"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID+"/attempts"),
		nil, "", http.StatusOK, &resp)
	return resp.Items
}

// attemptCount returns how many attempts jobID's tasks have, in all.
func attemptCount(t *testing.T, ts *testServer, jobID string) int {
	t.Helper()
	n := 0
	for _, task := range jobTasks(t, ts, jobID) {
		n += len(taskAttempts(t, ts, task.ID))
	}
	return n
}

// assertNoInFlightTasks asserts that every job is terminal, every task of
// every job is terminal and every attempt is closed: nothing is stranded
// assigned or running, and nothing is left that retention (which deletes only
// terminal jobs) could neither delete nor advance.
func assertNoInFlightTasks(t *testing.T, ts *testServer) {
	t.Helper()
	var jobs struct {
		Items []jobResp `json:"items"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/jobs?limit=1000"), nil, "", http.StatusOK, &jobs)
	if len(jobs.Items) == 0 {
		t.Fatal("no jobs listed")
	}
	terminalJob := map[string]bool{"completed": true, "failed": true, "canceled": true}
	terminalTask := map[string]bool{"succeeded": true, "failed": true, "canceled": true}
	for _, job := range jobs.Items {
		if !terminalJob[job.Status] {
			t.Errorf("job %s is %q, want a terminal status", job.ID, job.Status)
		}
		for _, task := range jobTasks(t, ts, job.ID) {
			if !terminalTask[task.Status] {
				t.Errorf("job %s task %s is %q, want a terminal status", job.ID, task.ID, task.Status)
			}
			for i, a := range taskAttempts(t, ts, task.ID) {
				if a.Status == "running" || a.EndedAt == nil {
					t.Errorf("job %s task %s attempt %d is %q (ended %v), want closed", job.ID, task.ID, i+1, a.Status, a.EndedAt)
				}
			}
		}
	}
}

// taskLogText returns all of taskID's latest attempt's log text.
func taskLogText(t *testing.T, ts *testServer, taskID string) string {
	t.Helper()
	var resp struct {
		Items []struct {
			Data string `json:"data"`
		} `json:"items"`
	}
	mustDoJSONClient(t, clientFor(ts), http.MethodGet, apiURL(ts, "/api/v1/tasks/"+taskID+"/logs?limit=1000"),
		nil, "", http.StatusOK, &resp)
	var sb strings.Builder
	for _, item := range resp.Items {
		sb.WriteString(item.Data)
	}
	return sb.String()
}

// jobLogText returns the log text of every task of jobID, concatenated.
func jobLogText(t *testing.T, ts *testServer, jobID string) string {
	t.Helper()
	var sb strings.Builder
	for _, task := range jobTasks(t, ts, jobID) {
		sb.WriteString(taskLogText(t, ts, task.ID))
	}
	return sb.String()
}

// waitJobLogs waits until every "done N" line of a sleepJob job is stored:
// logs are ingested by their own consumer, after the job may already be
// completed.
func waitJobLogs(t *testing.T, ts *testServer, jobID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		logs, missing := jobLogText(t, ts, jobID), 0
		for n := 1; n <= sleepJobTasks; n++ {
			if !strings.Contains(logs, fmt.Sprintf("done %d", n)) {
				missing++
			}
		}
		if missing == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: %d of %d done lines not stored after 30s; logs: %q", jobID, missing, sleepJobTasks, logs)
		}
		// Each round is 1+sleepJobTasks requests; once a second stays well
		// inside the API's per-IP limit (20 req/s, burst 40).
		time.Sleep(time.Second)
	}
}

// waitLogReplayDrained waits until the server's log consumer has nothing
// pending or unacknowledged — every chunk it was delivered is stored or
// discarded — and returns how many it has delivered since it was created.
func waitLogReplayDrained(t *testing.T, ts *testServer) uint64 {
	t.Helper()
	nc, err := nats.Connect("nats://" + ts.NATSAddr)
	if err != nil {
		t.Fatalf("connect to NATS: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		info, err := consumerInfo(ctx, js, bus.StreamLogs, logsConsumer)
		cancel()
		if err == nil && info.NumPending == 0 && info.NumAckPending == 0 {
			return info.Delivered.Consumer
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("pending %d, awaiting ack %d", info.NumPending, info.NumAckPending)
		}
		if time.Now().After(deadline) {
			t.Fatalf("log consumer not drained after 30s: %s", last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// consumerInfo returns the state of stream's durable consumer name.
func consumerInfo(ctx context.Context, js jetstream.JetStream, stream, name string) (*jetstream.ConsumerInfo, error) {
	c, err := js.Consumer(ctx, stream, name)
	if err != nil {
		return nil, err
	}
	return c.Info(ctx)
}

// dropConsumerState deletes the on-disk state of stream's durable consumer
// name from the stopped NATS server's data directory natsDir, so the next
// server recreates it and delivers the stream from the start. It fails the
// test unless it finds exactly one such directory: the JetStream file store
// keeps a consumer at .../streams/<stream>/obs/<consumer>.
func dropConsumerState(t *testing.T, natsDir, stream, name string) {
	t.Helper()
	var found []string
	err := filepath.WalkDir(natsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		obs := filepath.Dir(p)
		if d.IsDir() && d.Name() == name && filepath.Base(obs) == "obs" && filepath.Base(filepath.Dir(obs)) == stream {
			found = append(found, p)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d state directories for consumer %s of stream %s under %s, want 1: %v",
			len(found), name, stream, natsDir, found)
	}
	if err := os.RemoveAll(found[0]); err != nil {
		t.Fatal(err)
	}
}

// metricValue returns the value of the unlabeled metric name from ts's
// /metrics.
func metricValue(t *testing.T, ts *testServer, name string) float64 {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, apiURL(ts, "/metrics"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(ts).Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("metric %s: %v", name, err)
			}
			return f
		}
	}
	t.Fatalf("metric %s not in /metrics", name)
	return 0
}

// copyDir copies the directory tree src to dst, which must not exist.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.Mkdir(to, 0o700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("copyDir: %s is not a regular file or directory", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copy %s to %s: %v", src, dst, err)
	}
}

// copyFile copies src over dst.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
