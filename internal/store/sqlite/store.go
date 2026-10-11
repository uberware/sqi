// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sqlite provides the SQLite [store.Store]: it opens the database,
// applies its PRAGMAs, runs migrations, and builds the shared SQL store
// (internal/store/sqlstore) over its pools with the SQLite dialect. It also
// holds the operations only SQLite has: Ping, backup and WAL checkpointing.
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
// mode lets readers proceed alongside a writer, so a read does not queue
// behind one. A single large [Store.CreateJobSubmission] holds the write
// connection while it inserts every task, which takes seconds at the job sizes
// this server accepts; with one shared pool every other database user in the
// process, the health checker included, would stall for that long.
//
// The shared store routes by statement, not by call site: anything whose SQL
// begins with SELECT is a read, everything else is a write. Several writes are
// INSERT/UPDATE/DELETE ... RETURNING issued through QueryRow, so classifying by
// Go method would be wrong. Misclassifying a read as a write only serializes
// it; misclassifying a write as a read puts it on a multi-connection pool, so
// anything ambiguous (a CTE, say) stays on the write pool.
//
// Transactions always run on the write pool.
//
// Correctness under concurrent writers is not left to the single write
// connection: every multi-row invariant is a single store operation (see
// "Store invariants" in docs/architecture.md), and most of them name their
// anchor rows via Dialect.LockAnchors, which the SQLite dialect leaves a
// no-op because the one write connection already serializes every write
// transaction. A store with concurrent writers must lock those rows instead,
// and the anchors alone do not suffice there: ReclaimStaleAssignedTasks and
// DemoteStalledJobs name none, and docs/architecture.md lists the gaps the
// anchor table leaves open.
// Single-row writes carry their own precondition in their WHERE clause on
// either kind of store.
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
		p, err := migrations.NewProvider(db, "sqlite")
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite: migrations: %w", err)
		}
		if _, err := p.Up(ctx); err != nil {
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
	shared, err := sqlstore.New(ctx, db, rdb, sqliteDialect{})
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
// "mode=memory&cache=shared" is reachable from a second pool only while some
// connection stays open, so it is treated as private.
func isSharedPath(path string) bool {
	if path == "" || strings.Contains(path, ":memory:") {
		return false
	}
	return !strings.Contains(strings.ToLower(path), "mode=memory")
}

// Ping verifies the database connection is still alive. Health checks use it.
//
// It pings the read pool, not the write pool. Readiness reports whether the
// database is reachable, and a large job submission holds the sole write
// connection for seconds at a time; a ping queued behind it would turn a
// normal write into a failed health check.
func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.PingContext(ctx)
}
