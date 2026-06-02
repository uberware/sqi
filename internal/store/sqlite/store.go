// SPDX-License-Identifier: AGPL-3.0-only

// Package sqlite provides a SQLite-backed implementation of [store.Store].
//
// # Usage
//
//	opts := sqlite.DefaultOptions()
//	s, err := sqlite.Open(ctx, "/path/to/sqi.db", opts)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer s.Close()
//
// The returned [*Store] satisfies [store.Store] in full. It can be passed
// wherever a [store.Store] is expected, or used directly for the [Ping] method
// that is not part of the public interface (used by health checks).
//
// # Concurrency
//
// The underlying sql.DB is opened with [sql.DB.SetMaxOpenConns](1) which
// serializes all reads and writes through a single SQLite connection. This is
// the correct setting for SQLite in WAL mode when used from a single process:
// it prevents write-write conflicts and avoids the overhead of connection
// negotiation while still allowing the WAL reader snapshot semantics to
// function correctly within a single connection.
//
// # Migrations
//
// When [Options.AutoMigrate] is true (the default), [Open] calls goose.Up
// against the embedded migration FS before returning. Set AutoMigrate to false
// in HA deployments where migrations are applied explicitly with
// "sqi-server migrate up" before rolling out new server instances.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // register the "sqlite" driver

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
)

// Compile-time interface compliance check.
var _ store.Store = (*Store)(nil)

// Options configures [Open].
type Options struct {
	// AutoMigrate controls whether Open runs goose.Up to apply any pending
	// migrations before returning. Default: true.
	//
	// Set to false in HA deployments where migrations are applied explicitly
	// via "sqi-server migrate up" before starting new server instances.
	AutoMigrate bool
}

// DefaultOptions returns an [Options] with recommended defaults.
func DefaultOptions() Options {
	return Options{
		AutoMigrate: true,
	}
}

// Store is a SQLite-backed implementation of [store.Store].
// Create one with [Open]; close it with [Close] when done.
type Store struct {
	db    *sql.DB
	stmts []*sql.Stmt // all prepared statements; closed by Close

	// ── farms ────────────────────────────────────────────────────────────
	stmtInsertFarm *sql.Stmt
	stmtGetFarm    *sql.Stmt
	stmtListFarms  *sql.Stmt
	stmtUpdateFarm *sql.Stmt
	stmtDeleteFarm *sql.Stmt

	// ── queues ───────────────────────────────────────────────────────────
	stmtInsertQueue *sql.Stmt
	stmtGetQueue    *sql.Stmt
	stmtUpdateQueue *sql.Stmt
	stmtDeleteQueue *sql.Stmt

	// ── storage_locations ────────────────────────────────────────────────
	stmtInsertStorageLoc    *sql.Stmt
	stmtGetStorageLoc       *sql.Stmt
	stmtGetStorageLocByName *sql.Stmt
	stmtListStorageLocs     *sql.Stmt
	stmtUpdateStorageLoc    *sql.Stmt
	stmtDeleteStorageLoc    *sql.Stmt

	// ── license_pools ────────────────────────────────────────────────────
	stmtInsertPool *sql.Stmt
	stmtGetPool    *sql.Stmt
	stmtListPools  *sql.Stmt
	stmtUpdatePool *sql.Stmt
	stmtDeletePool *sql.Stmt

	// ── license_checkouts ────────────────────────────────────────────────
	stmtInsertCheckout          *sql.Stmt
	stmtReleaseCheckout         *sql.Stmt
	stmtActiveCheckoutCount     *sql.Stmt
	stmtReleaseAttemptCheckouts *sql.Stmt
	stmtReleaseJobCheckouts     *sql.Stmt

	// ── workers ──────────────────────────────────────────────────────────
	stmtUpsertWorker             *sql.Stmt
	stmtGetWorker                *sql.Stmt
	stmtUpdateWorker             *sql.Stmt
	stmtUpdateWorkerStatus       *sql.Stmt
	stmtUpdateWorkerHeartbeat    *sql.Stmt
	stmtListStaleWorkers         *sql.Stmt
	stmtCountIdleWorkers         *sql.Stmt
	stmtCountIdleWorkersAllFarms *sql.Stmt

	// ── jobs ─────────────────────────────────────────────────────────────
	stmtInsertJob       *sql.Stmt
	stmtGetJob          *sql.Stmt
	stmtUpdateJob       *sql.Stmt
	stmtUpdateJobStatus *sql.Stmt

	// ── steps ────────────────────────────────────────────────────────────
	stmtInsertStep       *sql.Stmt
	stmtGetStep          *sql.Stmt
	stmtListSteps        *sql.Stmt
	stmtUpdateStepStatus *sql.Stmt

	// ── tasks ────────────────────────────────────────────────────────────
	stmtInsertTask              *sql.Stmt
	stmtGetTask                 *sql.Stmt
	stmtUpdateTaskStatus        *sql.Stmt
	stmtAssignTask              *sql.Stmt
	stmtListReadyTasks          *sql.Stmt
	stmtReclaimWorkerTasks      *sql.Stmt
	stmtCountActiveTasksInQueue *sql.Stmt
	stmtCountActiveTasksInFarm  *sql.Stmt
	stmtCountReadyTasksByQueue  *sql.Stmt
	stmtCountTasksByJob         *sql.Stmt

	// ── task_attempts ────────────────────────────────────────────────────
	stmtInsertAttempt           *sql.Stmt
	stmtGetAttempt              *sql.Stmt
	stmtLatestAttempt           *sql.Stmt
	stmtListAttempts            *sql.Stmt
	stmtUpdateAttempt           *sql.Stmt
	stmtTerminateWorkerAttempts *sql.Stmt
	stmtCancelJobAttempts       *sql.Stmt

	// ── task_logs ────────────────────────────────────────────────────────
	stmtInsertTaskLog *sql.Stmt
	stmtListTaskLogs  *sql.Stmt

	// ── audit_log ────────────────────────────────────────────────────────
	stmtInsertAudit *sql.Stmt
}

// Open opens (or creates) the SQLite database at path, applies connection
// pragmas, optionally runs pending migrations, prepares all statements, and
// returns a ready-to-use [*Store].
//
// Open returns an error and releases all resources if any step fails.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlite: path is empty")
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}

	// Serialize all access through one connection. This is correct for
	// SQLite: WAL allows concurrent readers on a single connection and
	// serializing writes prevents SQLITE_BUSY under concurrent goroutines.
	db.SetMaxOpenConns(1)

	// Apply pragmas before any other operations.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err = db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite: set %s: %w", pragma, err)
		}
	}

	if opts.AutoMigrate {
		goose.SetBaseFS(migrations.FS)
		if err = goose.SetDialect("sqlite3"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite: set goose dialect: %w", err)
		}
		if err = goose.Up(db, "."); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite: migrate up: %w", err)
		}
	}

	s := &Store{db: db}
	if err = s.prepareAll(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Ping verifies the database connection is still alive. Used by health checks.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes all prepared statements and the underlying database connection.
// It is safe to call Close more than once.
func (s *Store) Close() error {
	var errs []error
	for _, stmt := range s.stmts {
		if err := stmt.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.db.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// prepare compiles sql into a prepared statement, tracks it for Close, and
// returns it. On failure it returns an error that includes the first 60 chars
// of the SQL for context.
func (s *Store) prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	stmt, err := s.db.PrepareContext(ctx, query)
	if err != nil {
		preview := query
		if len(preview) > 60 {
			preview = preview[:60] + "…"
		}
		return nil, fmt.Errorf("sqlite: prepare %q: %w", preview, err)
	}
	s.stmts = append(s.stmts, stmt)
	return stmt, nil
}

// prepareAll compiles every SQL statement used by the store. Errors are
// returned immediately; the caller (Open) releases resources via Close.
//
//nolint:cyclop,funlen // large but straightforward: one prepare call per query
func (s *Store) prepareAll(ctx context.Context) error {
	var err error

	// ── farms ─────────────────────────────────────────────────────────────
	if s.stmtInsertFarm, err = s.prepare(ctx, sqlInsertFarm); err != nil {
		return err
	}
	if s.stmtGetFarm, err = s.prepare(ctx, sqlGetFarm); err != nil {
		return err
	}
	if s.stmtListFarms, err = s.prepare(ctx, sqlListFarms); err != nil {
		return err
	}
	if s.stmtUpdateFarm, err = s.prepare(ctx, sqlUpdateFarm); err != nil {
		return err
	}
	if s.stmtDeleteFarm, err = s.prepare(ctx, sqlDeleteFarm); err != nil {
		return err
	}

	// ── queues ────────────────────────────────────────────────────────────
	if s.stmtInsertQueue, err = s.prepare(ctx, sqlInsertQueue); err != nil {
		return err
	}
	if s.stmtGetQueue, err = s.prepare(ctx, sqlGetQueue); err != nil {
		return err
	}
	if s.stmtUpdateQueue, err = s.prepare(ctx, sqlUpdateQueue); err != nil {
		return err
	}
	if s.stmtDeleteQueue, err = s.prepare(ctx, sqlDeleteQueue); err != nil {
		return err
	}

	// ── storage_locations ─────────────────────────────────────────────────
	if s.stmtInsertStorageLoc, err = s.prepare(ctx, sqlInsertStorageLoc); err != nil {
		return err
	}
	if s.stmtGetStorageLoc, err = s.prepare(ctx, sqlGetStorageLoc); err != nil {
		return err
	}
	if s.stmtGetStorageLocByName, err = s.prepare(ctx, sqlGetStorageLocByName); err != nil {
		return err
	}
	if s.stmtListStorageLocs, err = s.prepare(ctx, sqlListStorageLocs); err != nil {
		return err
	}
	if s.stmtUpdateStorageLoc, err = s.prepare(ctx, sqlUpdateStorageLoc); err != nil {
		return err
	}
	if s.stmtDeleteStorageLoc, err = s.prepare(ctx, sqlDeleteStorageLoc); err != nil {
		return err
	}

	// ── license_pools ─────────────────────────────────────────────────────
	if s.stmtInsertPool, err = s.prepare(ctx, sqlInsertPool); err != nil {
		return err
	}
	if s.stmtGetPool, err = s.prepare(ctx, sqlGetPool); err != nil {
		return err
	}
	if s.stmtListPools, err = s.prepare(ctx, sqlListPools); err != nil {
		return err
	}
	if s.stmtUpdatePool, err = s.prepare(ctx, sqlUpdatePool); err != nil {
		return err
	}
	if s.stmtDeletePool, err = s.prepare(ctx, sqlDeletePool); err != nil {
		return err
	}

	// ── license_checkouts ─────────────────────────────────────────────────
	if s.stmtInsertCheckout, err = s.prepare(ctx, sqlInsertCheckout); err != nil {
		return err
	}
	if s.stmtReleaseCheckout, err = s.prepare(ctx, sqlReleaseCheckout); err != nil {
		return err
	}
	if s.stmtActiveCheckoutCount, err = s.prepare(ctx, sqlActiveCheckoutCount); err != nil {
		return err
	}
	if s.stmtReleaseAttemptCheckouts, err = s.prepare(ctx, sqlReleaseAttemptCheckouts); err != nil {
		return err
	}
	if s.stmtReleaseJobCheckouts, err = s.prepare(ctx, sqlReleaseJobCheckouts); err != nil {
		return err
	}

	// ── workers ───────────────────────────────────────────────────────────
	if s.stmtUpsertWorker, err = s.prepare(ctx, sqlUpsertWorker); err != nil {
		return err
	}
	if s.stmtGetWorker, err = s.prepare(ctx, sqlGetWorker); err != nil {
		return err
	}
	if s.stmtUpdateWorker, err = s.prepare(ctx, sqlUpdateWorker); err != nil {
		return err
	}
	if s.stmtUpdateWorkerStatus, err = s.prepare(ctx, sqlUpdateWorkerStatus); err != nil {
		return err
	}
	if s.stmtUpdateWorkerHeartbeat, err = s.prepare(ctx, sqlUpdateWorkerHeartbeat); err != nil {
		return err
	}
	if s.stmtListStaleWorkers, err = s.prepare(ctx, sqlListStaleWorkers); err != nil {
		return err
	}
	if s.stmtCountIdleWorkers, err = s.prepare(ctx, sqlCountIdleWorkers); err != nil {
		return err
	}
	if s.stmtCountIdleWorkersAllFarms, err = s.prepare(ctx, sqlCountIdleWorkersAllFarms); err != nil {
		return err
	}

	// ── jobs ──────────────────────────────────────────────────────────────
	if s.stmtInsertJob, err = s.prepare(ctx, sqlInsertJob); err != nil {
		return err
	}
	if s.stmtGetJob, err = s.prepare(ctx, sqlGetJob); err != nil {
		return err
	}
	if s.stmtUpdateJob, err = s.prepare(ctx, sqlUpdateJob); err != nil {
		return err
	}
	if s.stmtUpdateJobStatus, err = s.prepare(ctx, sqlUpdateJobStatus); err != nil {
		return err
	}

	// ── steps ─────────────────────────────────────────────────────────────
	if s.stmtInsertStep, err = s.prepare(ctx, sqlInsertStep); err != nil {
		return err
	}
	if s.stmtGetStep, err = s.prepare(ctx, sqlGetStep); err != nil {
		return err
	}
	if s.stmtListSteps, err = s.prepare(ctx, sqlListSteps); err != nil {
		return err
	}
	if s.stmtUpdateStepStatus, err = s.prepare(ctx, sqlUpdateStepStatus); err != nil {
		return err
	}

	// ── tasks ─────────────────────────────────────────────────────────────
	if s.stmtInsertTask, err = s.prepare(ctx, sqlInsertTask); err != nil {
		return err
	}
	if s.stmtGetTask, err = s.prepare(ctx, sqlGetTask); err != nil {
		return err
	}
	if s.stmtUpdateTaskStatus, err = s.prepare(ctx, sqlUpdateTaskStatus); err != nil {
		return err
	}
	if s.stmtAssignTask, err = s.prepare(ctx, sqlAssignTask); err != nil {
		return err
	}
	if s.stmtListReadyTasks, err = s.prepare(ctx, sqlListReadyTasks); err != nil {
		return err
	}
	if s.stmtReclaimWorkerTasks, err = s.prepare(ctx, sqlReclaimWorkerTasks); err != nil {
		return err
	}
	if s.stmtCountActiveTasksInQueue, err = s.prepare(ctx, sqlCountActiveTasksInQueue); err != nil {
		return err
	}
	if s.stmtCountActiveTasksInFarm, err = s.prepare(ctx, sqlCountActiveTasksInFarm); err != nil {
		return err
	}
	if s.stmtCountReadyTasksByQueue, err = s.prepare(ctx, sqlCountReadyTasksByQueue); err != nil {
		return err
	}
	if s.stmtCountTasksByJob, err = s.prepare(ctx, sqlCountTasksByJob); err != nil {
		return err
	}

	// ── task_attempts ─────────────────────────────────────────────────────
	if s.stmtInsertAttempt, err = s.prepare(ctx, sqlInsertAttempt); err != nil {
		return err
	}
	if s.stmtGetAttempt, err = s.prepare(ctx, sqlGetAttempt); err != nil {
		return err
	}
	if s.stmtLatestAttempt, err = s.prepare(ctx, sqlLatestAttempt); err != nil {
		return err
	}
	if s.stmtListAttempts, err = s.prepare(ctx, sqlListAttempts); err != nil {
		return err
	}
	if s.stmtUpdateAttempt, err = s.prepare(ctx, sqlUpdateAttempt); err != nil {
		return err
	}
	if s.stmtTerminateWorkerAttempts, err = s.prepare(ctx, sqlTerminateWorkerAttempts); err != nil {
		return err
	}
	if s.stmtCancelJobAttempts, err = s.prepare(ctx, sqlCancelJobAttempts); err != nil {
		return err
	}

	// ── task_logs ─────────────────────────────────────────────────────────
	if s.stmtInsertTaskLog, err = s.prepare(ctx, sqlInsertTaskLog); err != nil {
		return err
	}
	if s.stmtListTaskLogs, err = s.prepare(ctx, sqlListTaskLogs); err != nil {
		return err
	}

	// ── audit_log ─────────────────────────────────────────────────────────
	if s.stmtInsertAudit, err = s.prepare(ctx, sqlInsertAudit); err != nil {
		return err
	}

	return nil
}
