// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

// White-box tests for the separate read connection pool. They live in package
// sqlite (not sqlite_test) because the whole point is which *sql.DB a statement
// lands on, and that is unexported.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// openFilePoolStore opens a file-backed store, which is the only kind that gets
// a genuinely separate read pool. t.Cleanup closes it.
func openFilePoolStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir()+"/test.db", DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	})
	return s
}

// seedFarmQueue creates the farm and queue a job row references.
func seedFarmQueue(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := s.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "Q1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
}

// ── statement classification ─────────────────────────────────────────────────

func TestIsReadStatement(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"plain select", `SELECT 1`, true},
		{"lowercase select", `select 1`, true},
		{"mixed case select", `SeLeCt 1`, true},
		{"leading whitespace and newline", "\n\t  SELECT id FROM jobs", true},
		{"select with paren", `SELECT(1)`, true},
		{"line comment then select", "-- a comment\nSELECT 1", true},
		{"two line comments then select", "-- one\n-- two\nSELECT 1", true},
		{"block comment then select", "/* a comment */ SELECT 1", true},
		{"insert", `INSERT INTO jobs (id) VALUES (?)`, false},
		{"insert returning", `INSERT INTO jobs (id) VALUES (?) RETURNING id`, false},
		{"update returning", `UPDATE jobs SET x = ? WHERE id = ? RETURNING id`, false},
		{"delete returning", `DELETE FROM workers WHERE id = ? RETURNING id`, false},
		{"pragma", `PRAGMA wal_checkpoint(TRUNCATE)`, false},
		{"vacuum into", `VACUUM INTO ?`, false},
		{"cte fails safe to the write pool", `WITH x AS (SELECT 1) SELECT * FROM x`, false},
		{"word starting with select", `SELECTED FROM x`, false},
		{"empty", ``, false},
		{"only a comment", "-- nothing here\n", false},
		{"unterminated block comment", "/* never closed SELECT 1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReadStatement(tt.query); got != tt.want {
				t.Errorf("isReadStatement(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestPoolFor_RoutesEveryPreparedStatement pins the routing of the actual SQL
// this package prepares. Writes that read like reads (INSERT/UPDATE/DELETE ...
// RETURNING, all issued through QueryRowContext) are the trap this guards: they
// must stay on the single-connection write pool.
func TestPoolFor_RoutesEveryPreparedStatement(t *testing.T) {
	s := openFilePoolStore(t)
	if s.rdb == s.db {
		t.Fatal("file-backed store did not get a separate read pool; routing is untestable")
	}

	reads := map[string]string{
		"sqlGetJob":                       sqlGetJob,
		"sqlListBlockedJobs":              sqlListBlockedJobs,
		"sqlListReadyTasks":               sqlListReadyTasks,
		"sqlCountTasksByJob":              sqlCountTasksByJob,
		"sqlCountUnschedulableTasksByJob": sqlCountUnschedulableTasksByJob,
		"sqlCommittedCores":               sqlCommittedCores,
		"sqlActiveClaimCount":             sqlActiveClaimCount,
		"sqlListPoolUsage":                sqlListPoolUsage,
		"sqlGetSessionUserByTokenHash":    sqlGetSessionUserByTokenHash,
		"sqlGetAPIKeyUserByTokenHash":     sqlGetAPIKeyUserByTokenHash,
		"sqlListTaskLogs":                 sqlListTaskLogs,
		"sqlListAuditAll":                 sqlListAuditAll,
		"sqlSelectTaskStatus":             sqlSelectTaskStatus,
		"sqlFailureReasonSummary":         sqlFailureReasonSummary,
		"sqlListJobDependencyIDs":         sqlListJobDependencyIDs,
	}
	writes := map[string]string{
		"sqlInsertJob":                  sqlInsertJob,
		"sqlInsertTask":                 sqlInsertTask,
		"sqlUpdateTaskStatus":           sqlUpdateTaskStatus,
		"sqlDeleteJobRow":               sqlDeleteJobRow,
		"sqlDeleteOfflineWorkersBefore": sqlDeleteOfflineWorkersBefore, // DELETE ... RETURNING
		"sqlUpdateUser":                 sqlUpdateUser,                 // UPDATE ... RETURNING
		"sqlSetUserDisplayName":         sqlSetUserDisplayName,         // UPDATE ... RETURNING
		"sqlIncrementJobFailure":        sqlIncrementJobFailure,        // UPDATE ... RETURNING
		"sqlRecordTaskFailure":          sqlRecordTaskFailure,          // UPDATE ... RETURNING
		"sqlUpsertWorker":               sqlUpsertWorker,               // INSERT ... RETURNING
		"sqlInsertAPIKey":               sqlInsertAPIKey,               // INSERT ... RETURNING
		"sqlUpdateComputeLoc":           sqlUpdateComputeLoc,           // UPDATE ... RETURNING
		"sqlDemoteStalledJobs":          sqlDemoteStalledJobs,          // UPDATE ... RETURNING
		"sqlTransitionStepPendingTasks": sqlTransitionStepPendingTasks, // UPDATE ... RETURNING
		"sqlLeaseReadyTask":             sqlLeaseReadyTask,
		"sqlRetryTasksPrefix":           sqlRetryTasksPrefix,
	}

	for name, query := range reads {
		if s.poolFor(query) != s.rdb {
			t.Errorf("%s routed to the write pool, want the read pool", name)
		}
	}
	for name, query := range writes {
		if s.poolFor(query) != s.db {
			t.Errorf("%s routed to the READ pool; a write on a multi-connection pool risks write-write conflicts", name)
		}
	}
}

// ── pool construction ────────────────────────────────────────────────────────

func TestOpen_FileBackedStoreGetsSeparateReadPool(t *testing.T) {
	s := openFilePoolStore(t)
	if s.rdb == nil {
		t.Fatal("rdb is nil")
	}
	if s.rdb == s.db {
		t.Error("rdb aliases db; reads would still serialize behind writes")
	}
}

// TestOpen_InMemoryStoreSharesOnePool pins that an in-memory database keeps a
// single pool: a second sql.DB on ":memory:" would open a different, empty
// database, so sharing is the only correct answer.
func TestOpen_InMemoryStoreSharesOnePool(t *testing.T) {
	for _, path := range []string{":memory:", "file::memory:", "file:x?mode=memory&cache=shared"} {
		t.Run(path, func(t *testing.T) {
			s, err := Open(context.Background(), path, DefaultOptions())
			if err != nil {
				t.Fatalf("Open(%q): %v", path, err)
			}
			defer func() {
				if closeErr := s.Close(); closeErr != nil {
					t.Errorf("Close: %v", closeErr)
				}
			}()
			if s.rdb != s.db {
				t.Errorf("Open(%q) created a second pool on a memory database", path)
			}
			// The store must still work end to end.
			if _, err := s.CreateFarm(context.Background(), store.Farm{ID: "f1", Name: "F1"}); err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			farms, err := s.ListFarms(context.Background())
			if err != nil {
				t.Fatalf("ListFarms: %v", err)
			}
			if len(farms) != 1 {
				t.Errorf("ListFarms returned %d farms, want 1", len(farms))
			}
		})
	}
}

// TestReadPool_BusyTimeoutOnEveryConnection pins that busy_timeout is set via a
// DSN parameter rather than a one-off PRAGMA after sql.Open: a PRAGMA applies
// only to whichever connection happened to serve it, and the read pool has
// several. All of them are held open at once so each must answer for itself.
func TestReadPool_BusyTimeoutOnEveryConnection(t *testing.T) {
	s := openFilePoolStore(t)
	// Bounded so a regression that collapses the read pool back onto the
	// single write connection fails here rather than deadlocking.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conns := make([]*sql.Conn, 0, readPoolMaxConns)
	defer func() {
		for _, c := range conns {
			if err := c.Close(); err != nil {
				t.Errorf("conn.Close: %v", err)
			}
		}
	}()
	for i := range readPoolMaxConns {
		c, err := s.rdb.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		conns = append(conns, c)
		var ms int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
			t.Fatalf("PRAGMA busy_timeout on conn %d: %v", i, err)
		}
		if ms != busyTimeoutMS {
			t.Errorf("read conn %d has busy_timeout %d, want %d", i, ms, busyTimeoutMS)
		}
	}
}

// ── read-your-writes ─────────────────────────────────────────────────────────

// TestReadPool_ReadsSeeCommittedWrites pins that the read pool is not stuck on
// a stale WAL snapshot: a row committed on the write pool is visible to the
// very next read, through both a prepared SELECT and a raw one.
func TestReadPool_ReadsSeeCommittedWrites(t *testing.T) {
	s := openFilePoolStore(t)
	ctx := context.Background()
	seedFarmQueue(t, s)

	for i := range 3 {
		id := fmt.Sprintf("j%d", i)
		if _, err := s.CreateJob(ctx, store.Job{
			ID: id, FarmID: "f1", QueueID: "q1", Name: id, Status: store.JobStatusPending,
		}); err != nil {
			t.Fatalf("CreateJob %s: %v", id, err)
		}

		// Prepared SELECT (read pool).
		got, err := s.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if got.ID != id {
			t.Errorf("GetJob returned %q, want %q", got.ID, id)
		}

		// Raw SELECT + COUNT(*) pagination path (read pool).
		page, err := s.ListJobs(ctx, store.ListJobsOptions{})
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		if page.Total != i+1 {
			t.Errorf("ListJobs total = %d after %d writes, want %d", page.Total, i+1, i+1)
		}
	}
}

// ── the point of the whole change ────────────────────────────────────────────

const (
	// readDeadline is how long a read may take while a write is in flight.
	// Generous next to the write it runs against, tight next to the 5s health
	// checker timeout this change exists to protect.
	readDeadline = 500 * time.Millisecond
	// heldWriteDuration is how long the deterministic test pins the single
	// write connection.
	heldWriteDuration = 2 * time.Second
)

// timeRead runs fn and returns how long it took, failing the test if it errors
// or exceeds readDeadline.
func timeRead(t *testing.T, what string, fn func(context.Context) error) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readDeadline)
	defer cancel()
	start := time.Now()
	err := fn(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Errorf("%s failed after %v: %v", what, elapsed, err)
		return elapsed
	}
	if elapsed > readDeadline {
		t.Errorf("%s took %v, want under %v", what, elapsed, readDeadline)
	}
	return elapsed
}

// readAll exercises Ping (what the health checker calls), a prepared SELECT and
// a raw SELECT, and reports the total time.
func readAll(t *testing.T, s *Store) time.Duration {
	t.Helper()
	total := timeRead(t, "Ping", s.Ping)
	total += timeRead(t, "ListFarms (prepared SELECT)", func(ctx context.Context) error {
		_, err := s.ListFarms(ctx)
		return err
	})
	total += timeRead(t, "ListJobs (raw SELECT)", func(ctx context.Context) error {
		_, err := s.ListJobs(ctx, store.ListJobsOptions{})
		return err
	})
	return total
}

// TestReadPool_ReadsProceedWhileWriteConnectionIsHeld is the deterministic half
// of the guarantee: the write pool holds exactly one connection, so an open
// write transaction owns it outright. Reads must not queue behind it.
//
// This does not depend on machine speed — the write connection is provably
// occupied for heldWriteDuration.
func TestReadPool_ReadsProceedWhileWriteConnectionIsHeld(t *testing.T) {
	s := openFilePoolStore(t)
	ctx := context.Background()
	seedFarmQueue(t, s)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	// Write inside the transaction so it holds the WAL writer lock too, not
	// just the Go-side connection.
	if _, err := tx.ExecContext(ctx, `UPDATE farms SET description = 'held' WHERE id = ?`, "f1"); err != nil {
		_ = tx.Rollback() //nolint:errcheck // test cleanup on an already-failing path
		t.Fatalf("update inside tx: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(heldWriteDuration)
		_ = tx.Rollback() //nolint:errcheck // rollback of a read-only-effect fixture tx
	}()

	elapsed := readAll(t, s)
	t.Logf("three reads took %v while the write connection was held for %v", elapsed, heldWriteDuration)
	<-done
}

// TestReadPool_ReadsProceedDuringLargeJobSubmission is the realistic half: a
// real CreateJobSubmission, with reads issued while it is in flight.
//
// The assertion that matters is a ratio, not a wall-clock number: the reads
// must take a small fraction of the write they overlap. That holds on any
// machine, because a faster machine shortens both. A wall-clock floor guards
// the one way it could pass vacuously — a write so quick the reads landed
// after it — and fails asking for a bigger submission rather than passing.
//
// taskCount is deliberately modest. The whole mechanism is pinned
// machine-independently by ReadsProceedWhileWriteConnectionIsHeld above; this
// test's job is to show the same thing happens on the real submission path,
// and a bigger job would only buy CI minutes under the race detector.
func TestReadPool_ReadsProceedDuringLargeJobSubmission(t *testing.T) {
	const (
		taskCount = 5000
		// The reads must fit in a fifth of the write, and the write must be
		// long enough to be a meaningful window at all.
		maxReadFraction = 5
		minWriteTime    = 100 * time.Millisecond
	)

	s := openFilePoolStore(t)
	ctx := context.Background()
	seedFarmQueue(t, s)

	sub := store.JobSubmission{
		Job:   store.Job{ID: "big", FarmID: "f1", QueueID: "q1", Name: "big", Status: store.JobStatusPending},
		Steps: []store.Step{{ID: "s1", JobID: "big", Name: "s", StepOrder: 0, Status: store.StepStatusReady}},
		Tasks: make([]store.Task, 0, taskCount),
	}
	for i := range taskCount {
		name := fmt.Sprintf("t-%d", i)
		sub.Tasks = append(sub.Tasks, store.Task{
			ID: name, JobID: "big", StepID: "s1", Name: name, Status: store.TaskStatusReady,
		})
	}

	var writeElapsed time.Duration
	writeErr := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		start := time.Now()
		close(started)
		_, err := s.CreateJobSubmission(ctx, sub)
		writeElapsed = time.Since(start)
		writeErr <- err
	}()

	<-started
	// Let the transaction actually open and start inserting.
	time.Sleep(20 * time.Millisecond)
	readElapsed := readAll(t, s)

	if err := <-writeErr; err != nil {
		t.Fatalf("CreateJobSubmission: %v", err)
	}
	t.Logf("write of %d tasks took %v; three concurrent reads took %v", taskCount, writeElapsed, readElapsed)

	if writeElapsed < minWriteTime {
		t.Fatalf("the write finished in %v (< %v), so the reads proved nothing; raise taskCount",
			writeElapsed, minWriteTime)
	}
	if readElapsed*maxReadFraction >= writeElapsed {
		t.Errorf("three reads took %v against a %v write (over 1/%d of it); they queued behind it",
			readElapsed, writeElapsed, maxReadFraction)
	}
}
