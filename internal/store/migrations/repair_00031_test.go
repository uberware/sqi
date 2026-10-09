// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

// claimSeed is one claim the 00031 tests plant: the state of the task and
// attempt it hangs off, and whether it is released before the migration runs.
type claimSeed struct {
	taskStatus    store.TaskStatus
	attemptStatus store.AttemptStatus
	released      bool
}

// seedClaimOn creates farm, queue, job, step, task (taskStatus), attempt
// (attemptStatus) and a claim on the pool, returning the claim ID. The claim is
// released afterwards when seed.released is set. A state production can reach
// is reached through production writes: a healthy claim (a running task whose
// attempt is running) is a lease and a start, and a released claim is that
// lease driven to its terminal report, which releases it. Every other seed is a
// leak, exactly a combination the guarded store operations no longer produce,
// so seedLeakedClaim injects it.
func seedClaimOn(t *testing.T, s *sqlite.Store, seed claimSeed, poolID string) string {
	t.Helper()
	healthy := seed.taskStatus == store.TaskStatusRunning && seed.attemptStatus == store.AttemptStatusRunning
	if !seed.released && !healthy {
		return seedLeakedClaim(t, s, seed, poolID)
	}
	task := submitJob(t, s, store.JobStatusRunning, store.StepStatusReady, store.TaskStatusReady).Tasks[0]
	attempt, claimID := leaseWithClaim(t, s, task.ID, poolID)
	storetest.Start(t, s, attempt)
	if !seed.released {
		return claimID
	}
	res, err := s.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
		AttemptID: attempt.ID, TaskID: task.ID,
		TaskStatus: seed.taskStatus, AttemptStatus: seed.attemptStatus, EndedAt: time.Now().UTC(),
	})
	if err != nil || !res.Applied {
		t.Fatalf("CompleteTaskAttempt (%s, %s) = %+v, %v; want it applied", seed.taskStatus, seed.attemptStatus, res, err)
	}
	return claimID
}

// seedLeakedClaim submits the job, step and task (taskStatus) and injects an
// attempt (attemptStatus) with a claim still active on it: the leak 00031
// repairs. The injectors are the only way to write it, because production
// closes an attempt and releases its claims together.
func seedLeakedClaim(t *testing.T, s *sqlite.Store, seed claimSeed, poolID string) string {
	t.Helper()
	task := submitJob(t, s, store.JobStatusRunning, store.StepStatusReady, seed.taskStatus).Tasks[0]
	attempt := injectAttempt(t, s, task.ID, 1, seed.attemptStatus)
	return storetest.InjectClaim(t, s, store.UsageClaim{PoolID: poolID, TaskAttemptID: attempt.ID}).ID
}

// leaseWithClaim leases taskID to a worker with one claim on poolID, the way
// the scheduler does, and returns the attempt the lease created and the claim's
// ID. The task is left assigned.
func leaseWithClaim(t *testing.T, s *sqlite.Store, taskID, poolID string) (attempt store.TaskAttempt, claimID string) {
	t.Helper()
	claimID = uuid.NewString()
	attempt = storetest.Lease(t, s, store.LeaseRequest{
		TaskID: taskID, WorkerID: "w",
		Claims: []store.UsagePoolClaim{{ClaimID: claimID, PoolID: poolID, PoolName: "lic"}},
	})
	return attempt, claimID
}

// injectAttempt writes attempt number of taskID in the given status exactly as
// given, through the store's injector: no lease, no state check. A closed
// attempt carries the ended_at a production close stamps, so the only thing
// unreachable about it is the combination it is planted in.
func injectAttempt(t *testing.T, s *sqlite.Store, taskID string, number int, status store.AttemptStatus) store.TaskAttempt {
	t.Helper()
	now := time.Now().UTC()
	attempt := store.TaskAttempt{TaskID: taskID, WorkerID: "w", AttemptNumber: number, Status: status, StartedAt: now}
	if status != store.AttemptStatusRunning {
		attempt.EndedAt = &now
	}
	return storetest.InjectAttempt(t, s, attempt)
}

// openSeedable opens a fresh migrated store, which carries every migration the
// tree has (00031 included, as a no-op on an empty database), and a pool for
// claims to be taken on.
func openSeedable(t *testing.T) (path string, s *sqlite.Store, poolID string) {
	t.Helper()
	path = t.TempDir() + "/test.db"
	s, err := sqlite.Open(t.Context(), path, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	pool, err := s.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "lic", MaxConcurrent: 10})
	if err != nil {
		_ = s.Close()
		t.Fatalf("CreateUsagePool: %v", err)
	}
	return path, s, pool.ID
}

// dumpTables renders every application table deterministically, for a
// byte-for-byte before/after comparison. goose's own bookkeeping table is
// excluded because rewinding and re-applying a version rewrites it by design.
func dumpTables(t *testing.T, db *sql.DB) string {
	t.Helper()
	tables := queryStrings(t, db,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'goose%' AND name NOT LIKE 'sqlite_%'
		 ORDER BY name`)
	var b strings.Builder
	for _, tbl := range tables {
		b.WriteString(dumpTable(t, db, tbl))
	}
	return b.String()
}

// dumpTable renders every row of one table in rowid order.
func dumpTable(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	// The table name comes from sqlite_master, never from input.
	rows, err := db.QueryContext(t.Context(), `SELECT * FROM "`+table+`" ORDER BY rowid`)
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("dump %s: columns: %v", table, err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var b strings.Builder
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("dump %s: scan: %v", table, err)
		}
		fmt.Fprintf(&b, "%s %v\n", table, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump %s: rows: %v", table, err)
	}
	return b.String()
}

// queryStrings runs a query returning one text column and collects it.
func queryStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("query %q: scan: %v", query, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query %q: rows: %v", query, err)
	}
	return out
}

// claimReleasedAt reads a claim's raw released_at text. No sqlite method parses
// that column (nothing reads it back into a Go value), so the test reads it
// directly and parses it with the layout the rest of the store writes.
func claimReleasedAt(t *testing.T, db *sql.DB, id string) sql.NullString {
	t.Helper()
	var released sql.NullString
	if err := db.QueryRowContext(t.Context(),
		`SELECT released_at FROM usage_claims WHERE id = ?`, id).Scan(&released); err != nil {
		t.Fatalf("read claim %s: %v", id, err)
	}
	return released
}

// rewindTo reopens the database file raw and runs goose back to version, so the
// next goose.Up re-applies every later migration to whatever was seeded. Every
// later migration's Down must leave the rows dumpTables compares untouched:
// 00032 drops and 00034 re-adds only an index or a column, and the data
// repairs' Downs are no-ops.
func rewindTo(t *testing.T, path string, version int64) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.DownTo(db, ".", version); err != nil {
		t.Fatalf("DownTo(%d): %v", version, err)
	}
	return db
}

func TestMigration00031_ReleasesLeakedClaims(t *testing.T) {
	path, s, poolID := openSeedable(t)
	healthy := seedClaimOn(t, s, claimSeed{store.TaskStatusRunning, store.AttemptStatusRunning, false}, poolID)
	leaked := []string{
		// Offline reclaim reset the task and closed the attempt, but left the claim.
		seedClaimOn(t, s, claimSeed{store.TaskStatusReady, store.AttemptStatusFailed, false}, poolID),
		// A cancel closed the attempt and the task, but left the claim.
		seedClaimOn(t, s, claimSeed{store.TaskStatusCanceled, store.AttemptStatusCanceled, false}, poolID),
		// A lease claimed a slot after the cancel released the job's claims.
		seedClaimOn(t, s, claimSeed{store.TaskStatusCanceled, store.AttemptStatusRunning, false}, poolID),
		// A terminal report whose claim release never landed: v0.3.0 closed the
		// attempt and moved the task, then released the claims in a separate call
		// whose error it treated as non-fatal. (This is not the v0.3.0 reaper
		// race, which left the older attempt running, claims active, on a task
		// still in flight, and which this predicate leaves alone.)
		seedClaimOn(t, s, claimSeed{store.TaskStatusSucceeded, store.AttemptStatusSucceeded, false}, poolID),
	}
	// An already-released claim on a finished attempt is neither healthy-active
	// nor leaked, and the repair must leave its released_at as it is.
	alreadyReleased := seedClaimOn(t, s, claimSeed{store.TaskStatusSucceeded, store.AttemptStatusSucceeded, true}, poolID)

	// The fixture really holds the damage: the I3 checker names exactly the four
	// leaked claims, so the assertions below are not vacuous.
	violations, err := s.ClaimInvariantViolations(t.Context())
	if err != nil {
		t.Fatalf("ClaimInvariantViolations (before): %v", err)
	}
	if want := slices.Sorted(slices.Values(leaked)); !slices.Equal(violations, want) {
		t.Fatalf("seeded violations = %v, want %v", violations, want)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := rewindTo(t, path, 30)
	releasedBefore := claimReleasedAt(t, db, alreadyReleased)
	if !releasedBefore.Valid {
		t.Fatalf("seeded already-released claim %s has no released_at", alreadyReleased)
	}

	// Apply 00031 alone. A full goose.Up would also run 00035, whose second
	// statement releases every active claim on a closed attempt (and whose
	// first closes the open attempt of a terminal task), so it would repair
	// these four leaks itself and this test would pass with 00031's UPDATE
	// removed.
	ranFrom := time.Now().UTC().Add(-time.Second)
	if err := goose.UpTo(db, ".", 31); err != nil {
		t.Fatalf("goose.UpTo(31): %v", err)
	}
	ranTo := time.Now().UTC().Add(time.Second)

	active := queryStrings(t, db, `SELECT id FROM usage_claims WHERE released_at IS NULL ORDER BY id`)
	if !slices.Equal(active, []string{healthy}) {
		t.Fatalf("active claims after 00031 = %v, want only the healthy %s (leaked: %v)", active, healthy, leaked)
	}

	// Every repaired claim carries a released_at the Go scanners can read: the
	// store parses timestamps with time.RFC3339Nano (see timeToText/textToTime),
	// and the migration writes them from SQLite's own clock.
	for _, id := range leaked {
		released := claimReleasedAt(t, db, id)
		if !released.Valid {
			t.Fatalf("leaked claim %s still has no released_at", id)
		}
		at, err := time.Parse(time.RFC3339Nano, released.String)
		if err != nil {
			t.Fatalf("claim %s released_at %q does not parse as RFC3339Nano: %v", id, released.String, err)
		}
		if at.Before(ranFrom) || at.After(ranTo) {
			t.Errorf("claim %s released_at = %s, want the migration time (between %s and %s)", id, at, ranFrom, ranTo)
		}
	}
	if got := claimReleasedAt(t, db, alreadyReleased); got != releasedBefore {
		t.Errorf("already-released claim's released_at changed: %v -> %v", releasedBefore, got)
	}

	// A normal open over the repaired file sees the invariant restored and the
	// healthy claim still holding its slot.
	if err := db.Close(); err != nil {
		t.Fatalf("Close raw db: %v", err)
	}
	s2, err := sqlite.Open(t.Context(), path, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if violations, err := s2.ClaimInvariantViolations(t.Context()); err != nil || len(violations) != 0 {
		t.Fatalf("ClaimInvariantViolations (after) = %v, %v; want none", violations, err)
	}
	if n, err := s2.ActiveClaimCount(t.Context(), poolID); err != nil || n != 1 {
		t.Fatalf("ActiveClaimCount (after) = %d, %v; want 1", n, err)
	}
}

func TestMigration00031_HealthyDatabaseUnchanged(t *testing.T) {
	path, s, poolID := openSeedable(t)
	seedClaimOn(t, s, claimSeed{store.TaskStatusRunning, store.AttemptStatusRunning, false}, poolID)
	// Released claims on finished attempts (succeeded and failed) are part of a
	// healthy farm too, and the repair must not touch them.
	seedClaimOn(t, s, claimSeed{store.TaskStatusSucceeded, store.AttemptStatusSucceeded, true}, poolID)
	seedClaimOn(t, s, claimSeed{store.TaskStatusFailed, store.AttemptStatusFailed, true}, poolID)
	violations, err := s.ClaimInvariantViolations(t.Context())
	if err != nil || len(violations) != 0 {
		t.Fatalf("seeded database is not healthy: violations = %v, err = %v", violations, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := rewindTo(t, path, 30)
	before := dumpTables(t, db)
	if before == "" {
		t.Fatal("dumpTables returned nothing; the comparison would be vacuous")
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	if after := dumpTables(t, db); after != before {
		t.Fatalf("00031 changed a healthy database:\n--- before\n%s--- after\n%s", before, after)
	}
}
