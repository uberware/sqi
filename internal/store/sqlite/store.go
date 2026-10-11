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
// Correctness under concurrent writers is not left to the single write
// connection: every multi-row invariant is a single store operation (see
// "Store invariants" in docs/architecture.md), and most of them name their
// anchor rows via Dialect.LockAnchors, a no-op here because the one write connection
// already serializes every write transaction. A store with concurrent writers
// must lock those rows instead, but the anchors alone are not yet enough
// there: ReclaimStaleAssignedTasks and DemoteStalledJobs name none, and the
// architecture doc lists the gaps the anchor table leaves open. Single-row
// writes carry their own precondition in their WHERE clause on either kind of
// store.
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
	"github.com/uberware/sqi/internal/store/sqlstore"
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

// Store is the SQLite state store: the shared SQL store over a single
// write connection and a read pool, plus SQLite's own operations.
type Store struct {
	*sqlstore.Store

	db  *sql.DB // write pool, one connection
	rdb *sql.DB // read pool; the write pool for an in-memory database
}

// Open opens (or creates) the SQLite database at path, applies connection
// pragmas, optionally runs pending migrations, prepares all statements, and
// returns a ready-to-use [*Store].
//
// Open returns an error and releases all resources if any step fails.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	return openWith(ctx, path, opts, sqliteDialect{})
}

// openWith is [Open] with the statements run through dialect d.
func openWith(ctx context.Context, path string, opts Options, d sqlstore.Dialect) (*Store, error) {
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
	rdb, err := openReadPool(path, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	shared, err := sqlstore.New(ctx, db, rdb, d)
	if err != nil {
		return nil, err
	}
	return &Store{Store: shared, db: db, rdb: rdb}, nil
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
