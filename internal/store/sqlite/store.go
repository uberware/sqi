// SPDX-License-Identifier: AGPL-3.0-or-later

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
// The store keeps two connection pools over the same database file.
//
// Writes go through a pool opened with [sql.DB.SetMaxOpenConns](1), which
// serializes every write through a single SQLite connection. That prevents
// write-write conflicts and avoids SQLITE_BUSY between concurrent goroutines.
//
// Reads go through a second pool of up to [readPoolMaxConns] connections. WAL
// mode lets readers proceed concurrently with a writer, so a read need not
// queue behind one — and must not: a single large [Store.CreateJobSubmission]
// holds the write connection for as long as it takes to insert every task,
// which at job sizes this server accepts is measured in seconds. With one
// shared pool that stalls every other database user in the process, including
// the health checker, so an ordinary large render job reads as an outage.
//
// The split is by statement, not by call site: anything whose SQL begins with
// SELECT is a read, everything else is a write. Several writes here are
// INSERT/UPDATE/DELETE ... RETURNING issued through QueryRow, so classifying by
// Go method would be wrong. Misclassifying a read as a write merely serializes
// it; misclassifying a write as a read puts it on a multi-connection pool, so
// anything ambiguous (a CTE, say) stays on the write pool.
//
// Transactions always run on the write pool.
//
// An in-memory or temporary database gets one pool for both roles: a second
// [sql.Open] on ":memory:" would open a different, empty database.
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
	"strings"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // register the "sqlite" driver

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
)

const (
	// readPoolMaxConns bounds the read pool. Four is enough to keep the health
	// check, the web UI's list views and the scheduler's lookups off each
	// other's critical path during a long write, while staying small enough
	// that a burst of API traffic cannot multiply SQLite's per-connection page
	// cache or the process's file-descriptor use without limit. Reads are
	// short; queuing briefly behind three of them is not the problem this
	// pool exists to solve.
	readPoolMaxConns = 4

	// busyTimeoutMS is the per-connection SQLITE_BUSY retry window, applied to
	// both pools.
	busyTimeoutMS = 5000
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
	// db is the write pool: exactly one connection, so writes never conflict.
	// Every transaction, and every statement that is not a SELECT, uses it.
	db *sql.DB
	// rdb is the read pool: up to readPoolMaxConns connections over the same
	// file, so a SELECT does not queue behind a long write. For an in-memory
	// or temporary database it aliases db — see [Open].
	rdb   *sql.DB
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

	// ── compute_locations ────────────────────────────────────────────────────
	stmtInsertComputeLoc    *sql.Stmt
	stmtGetComputeLoc       *sql.Stmt
	stmtGetComputeLocByName *sql.Stmt
	stmtListComputeLocs     *sql.Stmt
	stmtUpdateComputeLoc    *sql.Stmt
	stmtDeleteComputeLoc    *sql.Stmt

	// ── products ─────────────────────────────────────────────────────────────
	stmtInsertProduct    *sql.Stmt
	stmtGetProductByName *sql.Stmt
	stmtListProducts     *sql.Stmt
	stmtUpdateProduct    *sql.Stmt
	stmtDeleteProduct    *sql.Stmt

	// ── usage_pools ──────────────────────────────────────────────────────
	stmtInsertPool    *sql.Stmt
	stmtGetPool       *sql.Stmt
	stmtListPools     *sql.Stmt
	stmtListPoolUsage *sql.Stmt
	stmtUpdatePool    *sql.Stmt
	stmtDeletePool    *sql.Stmt

	// ── usage_claims ─────────────────────────────────────────────────────
	stmtInsertClaim          *sql.Stmt
	stmtReleaseClaim         *sql.Stmt
	stmtActiveClaimCount     *sql.Stmt
	stmtReleaseAttemptClaims *sql.Stmt
	stmtReleaseJobClaims     *sql.Stmt

	// ── workers ──────────────────────────────────────────────────────────
	stmtUpsertWorker             *sql.Stmt
	stmtGetWorker                *sql.Stmt
	stmtUpdateWorker             *sql.Stmt
	stmtUpdateWorkerStatus       *sql.Stmt
	stmtUpdateWorkerHeartbeat    *sql.Stmt
	stmtListStaleWorkers         *sql.Stmt
	stmtCountIdleWorkers         *sql.Stmt
	stmtCountIdleWorkersAllFarms *sql.Stmt
	stmtDeleteWorker             *sql.Stmt
	stmtDeleteOfflineWorkers     *sql.Stmt

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
	stmtInsertTask                   *sql.Stmt
	stmtGetTask                      *sql.Stmt
	stmtUpdateTaskStatus             *sql.Stmt
	stmtSetTaskUnschedulableReason   *sql.Stmt
	stmtSetTaskFailureReason         *sql.Stmt
	stmtSetTaskFailureReasonIfEmpty  *sql.Stmt
	stmtAssignTask                   *sql.Stmt
	stmtListReadyTasks               *sql.Stmt
	stmtReclaimWorkerTasks           *sql.Stmt
	stmtCountActiveTasksInQueue      *sql.Stmt
	stmtCountActiveTasksInFarm       *sql.Stmt
	stmtCountReadyTasksByQueue       *sql.Stmt
	stmtCountTasksByJob              *sql.Stmt
	stmtCountUnschedulableTasksByJob *sql.Stmt

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

	// ── users ────────────────────────────────────────────────────────────
	stmtInsertUser          *sql.Stmt
	stmtGetUser             *sql.Stmt
	stmtGetUserByUsername   *sql.Stmt
	stmtGetUserByExternalID *sql.Stmt
	stmtListUsers           *sql.Stmt
	stmtUpdateUser          *sql.Stmt
	stmtSetUserPassword     *sql.Stmt
	stmtSetUserDisplayName  *sql.Stmt
	stmtDeleteUser          *sql.Stmt
	stmtCountUsers          *sql.Stmt
	stmtCountAdmins         *sql.Stmt

	// ── sessions ─────────────────────────────────────────────────────────
	stmtInsertSession             *sql.Stmt
	stmtGetSessionByTokenHash     *sql.Stmt
	stmtGetSessionUserByTokenHash *sql.Stmt
	stmtDeleteSession             *sql.Stmt
	stmtDeleteSessionsForUser     *sql.Stmt
	stmtDeleteExpiredSessions     *sql.Stmt

	// ── api keys ─────────────────────────────────────────────────────────
	stmtInsertAPIKey             *sql.Stmt
	stmtGetAPIKeyByTokenHash     *sql.Stmt
	stmtGetAPIKeyUserByTokenHash *sql.Stmt
	stmtListAPIKeysForUser       *sql.Stmt
	stmtRevokeAPIKey             *sql.Stmt
	stmtTouchAPIKeyLastUsed      *sql.Stmt

	// ── worker credentials & join tokens ────────────────────────────────────
	stmtInsertWorkerCredential              *sql.Stmt
	stmtGetActiveWorkerCredentialByWorkerID *sql.Stmt
	stmtListActiveWorkerCredentials         *sql.Stmt
	stmtRevokeWorkerCredential              *sql.Stmt
	stmtInsertWorkerJoinToken               *sql.Stmt
	stmtGetWorkerJoinTokenByHash            *sql.Stmt
	stmtMarkWorkerJoinTokenUsed             *sql.Stmt
	stmtConsumeWorkerJoinToken              *sql.Stmt
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

	// Serialize every write through one connection: that is what prevents
	// write-write conflicts and SQLITE_BUSY between goroutines. Reads get
	// their own pool below; WAL lets them run alongside a writer.
	db.SetMaxOpenConns(1)

	// Apply pragmas before any other operations. journal_mode is persistent in
	// the file, so the read pool inherits WAL without re-applying it.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMS),
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

	// The read pool is opened only after migrations have run, so it can never
	// prepare a statement against a half-migrated schema.
	s := &Store{db: db}
	if s.rdb, err = openReadPool(path, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = s.prepareAll(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// openReadPool opens the second, multi-connection pool over the same database
// file. It returns write (the caller's write pool) unchanged when path names a
// database a second [sql.Open] could not reach — an in-memory or temporary
// database is private to the connection that created it, so a second pool
// would silently be a second, empty database.
func openReadPool(path string, write *sql.DB) (*sql.DB, error) {
	if !isSharedPath(path) {
		return write, nil
	}

	// busy_timeout is per-connection, so a one-off PRAGMA after sql.Open would
	// apply to whichever connection happened to serve it and to no other. The
	// DSN parameter is applied by the driver to every connection it opens.
	// Nothing else needs re-applying: journal_mode=WAL is persistent in the
	// file, and foreign_keys only constrains writes.
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	dsn := fmt.Sprintf("%s%s_pragma=busy_timeout(%d)", path, sep, busyTimeoutMS)

	rdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open read pool %q: %w", dsn, err)
	}
	rdb.SetMaxOpenConns(readPoolMaxConns)
	// Match idle to open so a steady read load does not churn connections:
	// the default idle limit is 2, which would tear down and rebuild the other
	// two on every burst.
	rdb.SetMaxIdleConns(readPoolMaxConns)
	return rdb, nil
}

// isSharedPath reports whether path names a database file that a second
// connection pool would see as the same database. In-memory databases
// (":memory:", "file::memory:", "mode=memory") and the anonymous temporary
// database (an empty path) are private per connection, so they are not.
//
// "mode=memory&cache=shared" is technically reachable from a second pool, but
// only while some connection stays open; it is treated as private rather than
// relying on that.
func isSharedPath(path string) bool {
	if path == "" || strings.Contains(path, ":memory:") {
		return false
	}
	return !strings.Contains(strings.ToLower(path), "mode=memory")
}

// Ping verifies the database connection is still alive. Used by health checks.
//
// It deliberately pings the READ pool, not the write pool. Readiness must
// report whether the database is reachable, and a large job submission holds
// the sole write connection for seconds at a time; pinging through it would
// turn a normal write into a failed health check. This is not an oversight —
// the health checker is not meant to observe write-pool queuing.
func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.PingContext(ctx)
}

// Close closes all prepared statements and both connection pools.
// It is safe to call Close more than once.
func (s *Store) Close() error {
	var errs []error
	for _, stmt := range s.stmts {
		if err := stmt.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// rdb aliases db for in-memory databases; closing it twice would error.
	if s.rdb != nil && s.rdb != s.db {
		if err := s.rdb.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.db.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// poolFor returns the pool a statement belongs on: the read pool for a SELECT,
// the write pool for everything else.
func (s *Store) poolFor(query string) *sql.DB {
	if isReadStatement(query) {
		return s.rdb
	}
	return s.db
}

// isReadStatement reports whether query is a plain SELECT, ignoring leading
// whitespace and SQL comments.
//
// It fails safe: anything it cannot positively identify as a SELECT — a CTE
// (WITH ... may wrap an INSERT or UPDATE just as easily as a SELECT), a
// PRAGMA, an unterminated comment — is treated as a write. Sending a read to
// the write pool only serializes it, which is what every statement did before
// the read pool existed; sending a write to the read pool would put it on a
// multi-connection pool and risk write-write conflicts.
func isReadStatement(query string) bool {
	const sel = "select"
	s := trimSQLLeader(query)
	if len(s) < len(sel) || !strings.EqualFold(s[:len(sel)], sel) {
		return false
	}
	if len(s) == len(sel) {
		return true
	}
	// Reject identifiers that merely start with "select" (SELECTED, ...).
	switch s[len(sel)] {
	case ' ', '\t', '\n', '\r', '\f', '\v', '(':
		return true
	default:
		return false
	}
}

// trimSQLLeader strips leading whitespace and SQL comments from query, so the
// first token of the statement itself is at the front of the result. An
// unterminated block comment consumes the rest of the string, which leaves
// isReadStatement with nothing to match — the safe answer.
func trimSQLLeader(query string) string {
	for {
		query = strings.TrimLeft(query, " \t\n\r\f\v")
		switch {
		case strings.HasPrefix(query, "--"):
			nl := strings.IndexByte(query, '\n')
			if nl < 0 {
				return ""
			}
			query = query[nl+1:]
		case strings.HasPrefix(query, "/*"):
			end := strings.Index(query[2:], "*/")
			if end < 0 {
				return ""
			}
			query = query[2+end+2:]
		default:
			return query
		}
	}
}

// prepare compiles sql into a prepared statement on the pool [Store.poolFor]
// selects, tracks it for Close, and returns it. On failure it returns an error
// that includes the first 60 chars of the SQL for context.
func (s *Store) prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	stmt, err := s.poolFor(query).PrepareContext(ctx, query)
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

	// ── compute_locations ─────────────────────────────────────────────────────
	if s.stmtInsertComputeLoc, err = s.prepare(ctx, sqlInsertComputeLoc); err != nil {
		return err
	}
	if s.stmtGetComputeLoc, err = s.prepare(ctx, sqlGetComputeLoc); err != nil {
		return err
	}
	if s.stmtGetComputeLocByName, err = s.prepare(ctx, sqlGetComputeLocByName); err != nil {
		return err
	}
	if s.stmtListComputeLocs, err = s.prepare(ctx, sqlListComputeLocs); err != nil {
		return err
	}
	if s.stmtUpdateComputeLoc, err = s.prepare(ctx, sqlUpdateComputeLoc); err != nil {
		return err
	}
	if s.stmtDeleteComputeLoc, err = s.prepare(ctx, sqlDeleteComputeLoc); err != nil {
		return err
	}

	// ── products ──────────────────────────────────────────────────────────
	if s.stmtInsertProduct, err = s.prepare(ctx, sqlInsertProduct); err != nil {
		return err
	}
	if s.stmtGetProductByName, err = s.prepare(ctx, sqlGetProductByName); err != nil {
		return err
	}
	if s.stmtListProducts, err = s.prepare(ctx, sqlListProducts); err != nil {
		return err
	}
	if s.stmtUpdateProduct, err = s.prepare(ctx, sqlUpdateProduct); err != nil {
		return err
	}
	if s.stmtDeleteProduct, err = s.prepare(ctx, sqlDeleteProduct); err != nil {
		return err
	}

	// ── usage_pools ───────────────────────────────────────────────────────
	if s.stmtInsertPool, err = s.prepare(ctx, sqlInsertPool); err != nil {
		return err
	}
	if s.stmtGetPool, err = s.prepare(ctx, sqlGetPool); err != nil {
		return err
	}
	if s.stmtListPools, err = s.prepare(ctx, sqlListPools); err != nil {
		return err
	}
	if s.stmtListPoolUsage, err = s.prepare(ctx, sqlListPoolUsage); err != nil {
		return err
	}
	if s.stmtUpdatePool, err = s.prepare(ctx, sqlUpdatePool); err != nil {
		return err
	}
	if s.stmtDeletePool, err = s.prepare(ctx, sqlDeletePool); err != nil {
		return err
	}

	// ── usage_claims ──────────────────────────────────────────────────────
	if s.stmtInsertClaim, err = s.prepare(ctx, sqlInsertClaim); err != nil {
		return err
	}
	if s.stmtReleaseClaim, err = s.prepare(ctx, sqlReleaseClaim); err != nil {
		return err
	}
	if s.stmtActiveClaimCount, err = s.prepare(ctx, sqlActiveClaimCount); err != nil {
		return err
	}
	if s.stmtReleaseAttemptClaims, err = s.prepare(ctx, sqlReleaseAttemptClaims); err != nil {
		return err
	}
	if s.stmtReleaseJobClaims, err = s.prepare(ctx, sqlReleaseJobClaims); err != nil {
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
	if s.stmtDeleteWorker, err = s.prepare(ctx, sqlDeleteWorker); err != nil {
		return err
	}
	if s.stmtDeleteOfflineWorkers, err = s.prepare(ctx, sqlDeleteOfflineWorkersBefore); err != nil {
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
	if s.stmtSetTaskUnschedulableReason, err = s.prepare(ctx, sqlSetTaskUnschedulableReason); err != nil {
		return err
	}
	if s.stmtSetTaskFailureReason, err = s.prepare(ctx, sqlSetTaskFailureReason); err != nil {
		return err
	}
	if s.stmtSetTaskFailureReasonIfEmpty, err = s.prepare(ctx, sqlSetTaskFailureReasonIfEmpty); err != nil {
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
	if s.stmtCountUnschedulableTasksByJob, err = s.prepare(ctx, sqlCountUnschedulableTasksByJob); err != nil {
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

	// ── users ─────────────────────────────────────────────────────────────
	if s.stmtInsertUser, err = s.prepare(ctx, sqlInsertUser); err != nil {
		return err
	}
	if s.stmtGetUser, err = s.prepare(ctx, sqlGetUser); err != nil {
		return err
	}
	if s.stmtGetUserByUsername, err = s.prepare(ctx, sqlGetUserByUsername); err != nil {
		return err
	}
	if s.stmtGetUserByExternalID, err = s.prepare(ctx, sqlGetUserByExternalID); err != nil {
		return err
	}
	if s.stmtListUsers, err = s.prepare(ctx, sqlListUsers); err != nil {
		return err
	}
	if s.stmtUpdateUser, err = s.prepare(ctx, sqlUpdateUser); err != nil {
		return err
	}
	if s.stmtSetUserPassword, err = s.prepare(ctx, sqlSetUserPassword); err != nil {
		return err
	}
	if s.stmtSetUserDisplayName, err = s.prepare(ctx, sqlSetUserDisplayName); err != nil {
		return err
	}
	if s.stmtDeleteUser, err = s.prepare(ctx, sqlDeleteUser); err != nil {
		return err
	}
	if s.stmtCountUsers, err = s.prepare(ctx, sqlCountUsers); err != nil {
		return err
	}
	if s.stmtCountAdmins, err = s.prepare(ctx, sqlCountAdmins); err != nil {
		return err
	}

	// ── sessions ──────────────────────────────────────────────────────────
	if s.stmtInsertSession, err = s.prepare(ctx, sqlInsertSession); err != nil {
		return err
	}
	if s.stmtGetSessionByTokenHash, err = s.prepare(ctx, sqlGetSessionByTokenHash); err != nil {
		return err
	}
	if s.stmtGetSessionUserByTokenHash, err = s.prepare(ctx, sqlGetSessionUserByTokenHash); err != nil {
		return err
	}
	if s.stmtDeleteSession, err = s.prepare(ctx, sqlDeleteSession); err != nil {
		return err
	}
	if s.stmtDeleteSessionsForUser, err = s.prepare(ctx, sqlDeleteSessionsForUser); err != nil {
		return err
	}
	if s.stmtDeleteExpiredSessions, err = s.prepare(ctx, sqlDeleteExpiredSessions); err != nil {
		return err
	}

	// ── api keys ──────────────────────────────────────────────────────────
	if s.stmtInsertAPIKey, err = s.prepare(ctx, sqlInsertAPIKey); err != nil {
		return err
	}
	if s.stmtGetAPIKeyByTokenHash, err = s.prepare(ctx, sqlGetAPIKeyByTokenHash); err != nil {
		return err
	}
	if s.stmtGetAPIKeyUserByTokenHash, err = s.prepare(ctx, sqlGetAPIKeyUserByTokenHash); err != nil {
		return err
	}
	if s.stmtListAPIKeysForUser, err = s.prepare(ctx, sqlListAPIKeysForUser); err != nil {
		return err
	}
	if s.stmtRevokeAPIKey, err = s.prepare(ctx, sqlRevokeAPIKey); err != nil {
		return err
	}
	if s.stmtTouchAPIKeyLastUsed, err = s.prepare(ctx, sqlTouchAPIKeyLastUsed); err != nil {
		return err
	}

	// ── worker credentials & join tokens ─────────────────────────────────────
	if s.stmtInsertWorkerCredential, err = s.prepare(ctx, sqlInsertWorkerCredential); err != nil {
		return err
	}
	if s.stmtGetActiveWorkerCredentialByWorkerID, err = s.prepare(ctx, sqlGetActiveWorkerCredentialByWorkerID); err != nil {
		return err
	}
	if s.stmtListActiveWorkerCredentials, err = s.prepare(ctx, sqlListActiveWorkerCredentials); err != nil {
		return err
	}
	if s.stmtRevokeWorkerCredential, err = s.prepare(ctx, sqlRevokeWorkerCredential); err != nil {
		return err
	}
	if s.stmtInsertWorkerJoinToken, err = s.prepare(ctx, sqlInsertWorkerJoinToken); err != nil {
		return err
	}
	if s.stmtGetWorkerJoinTokenByHash, err = s.prepare(ctx, sqlGetWorkerJoinTokenByHash); err != nil {
		return err
	}
	if s.stmtMarkWorkerJoinTokenUsed, err = s.prepare(ctx, sqlMarkWorkerJoinTokenUsed); err != nil {
		return err
	}
	if s.stmtConsumeWorkerJoinToken, err = s.prepare(ctx, sqlConsumeWorkerJoinToken); err != nil {
		return err
	}

	return nil
}
