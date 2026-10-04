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
// released afterwards when seed.released is set. The rows are written with raw
// store creators on purpose: a leaked claim is exactly a combination the
// guarded store operations no longer produce.
func seedClaimOn(t *testing.T, s *sqlite.Store, seed claimSeed, poolID string) string {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()

	farm, err := s.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := s.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	job, err := s.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: farm.ID, QueueID: queue.ID, Name: "j",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := s.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "s", Status: store.StepStatusReady, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	task, err := s.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t", Status: seed.taskStatus, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	attempt, err := s.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID: uuid.NewString(), TaskID: task.ID, WorkerID: "w", AttemptNumber: 1,
		Status: seed.attemptStatus, StartedAt: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	claim, err := s.CreateClaim(ctx, store.UsageClaim{ID: uuid.NewString(), PoolID: poolID, TaskAttemptID: attempt.ID})
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	if seed.released {
		if err := s.ReleaseClaim(ctx, claim.ID, now); err != nil {
			t.Fatalf("ReleaseClaim: %v", err)
		}
	}
	return claim.ID
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
		// F2: offline reclaim reset the task and closed the attempt, but left the claim.
		seedClaimOn(t, s, claimSeed{store.TaskStatusReady, store.AttemptStatusFailed, false}, poolID),
		// F3/F4: a cancel closed the attempt and the task, but left the claim.
		seedClaimOn(t, s, claimSeed{store.TaskStatusCanceled, store.AttemptStatusCanceled, false}, poolID),
		// F4: a lease claimed a slot after the cancel released the job's claims.
		seedClaimOn(t, s, claimSeed{store.TaskStatusCanceled, store.AttemptStatusRunning, false}, poolID),
		// A terminal report whose claim release never landed: v0.3.0 closed the
		// attempt and moved the task, then released the claims in a separate call
		// whose error it treated as non-fatal. (This is not F5: v0.3.0's F5 left
		// the older attempt running, claims active, on a task still in flight,
		// which this predicate rightly leaves alone.)
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

	ranFrom := time.Now().UTC().Add(-time.Second)
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
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
