// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/sqlstore"
)

var _ store.Store = (*Store)(nil)

// readPoolMaxConns is the read pool's size. Ping uses it, so readiness never
// queues behind writers holding every write connection.
const readPoolMaxConns = 4

// Options configures [Open].
type Options struct {
	// MaxConns is the write pool's size (store.postgres.max_conns).
	MaxConns int
	// AutoMigrate runs pending migrations before Open returns.
	AutoMigrate bool
	// ServerLock takes the single-server advisory lock for the store's life
	// (see ServerLockKey). The server sets it; the CLIs do not. The lock's
	// connection comes out of the write pool, so a server with MaxConns 16 has
	// 15 for its work, and ServerLock needs MaxConns of at least 2.
	ServerLock bool
	// ForceTxRetry rehearses every transaction once (sqlstore.WithForcedRetry).
	// Tests only.
	ForceTxRetry bool
	// Logger receives retry diagnostics and a lost server lock; slog.Default()
	// when nil.
	Logger *slog.Logger
	// WrapDialect, when set, wraps the dialect every statement goes through.
	// Tests only: the race tests pause a transaction at a chosen statement.
	WrapDialect func(sqlstore.Dialect) sqlstore.Dialect
}

// DefaultOptions returns the server's defaults: 16 write connections, migrate.
func DefaultOptions() Options { return Options{MaxConns: 16, AutoMigrate: true} }

// Store is the PostgreSQL state store.
type Store struct {
	*sqlstore.Store

	db, rdb *sql.DB
	lock    *serverLock // nil unless Options.ServerLock
	dsn     string
	log     *slog.Logger

	closeMu sync.Mutex
	closed  bool
}

// Open connects to the database dsn names, optionally migrates it and takes
// the server lock, and prepares every statement. Every connection runs READ
// COMMITTED whatever the server's default: the store's locking argument
// (docs/architecture.md, "Store invariants") depends on it.
func Open(ctx context.Context, dsn string, opts Options) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("postgres: dsn is empty")
	}
	if opts.MaxConns < 1 {
		opts.MaxConns = DefaultOptions().MaxConns
	}
	if opts.ServerLock && opts.MaxConns < 2 {
		return nil, fmt.Errorf("postgres: MaxConns %d leaves no write connection beside the server lock's; need at least 2", opts.MaxConns)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	db, err := openPool(dsn, opts.MaxConns)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	s := &Store{db: db, dsn: dsn, log: log}
	fail := func(err error) (*Store, error) {
		return nil, errors.Join(err, s.closePools())
	}
	if opts.ServerLock {
		if s.lock, err = acquireServerLock(ctx, db, log); err != nil {
			return fail(err)
		}
	}
	if err := checkSchemaVersion(ctx, db); err != nil {
		return fail(err)
	}
	if opts.AutoMigrate {
		if err := migrateUp(ctx, db); err != nil {
			return fail(err)
		}
	}
	if s.rdb, err = openPool(dsn, readPoolMaxConns); err != nil {
		return fail(err)
	}
	d, sopts := sharedConfig(opts, log)
	if s.Store, err = sqlstore.New(ctx, db, s.rdb, d, sopts...); err != nil {
		// New closed both pools itself; only the lock is left to release.
		s.db, s.rdb = nil, nil
		return fail(err)
	}
	return s, nil
}

// checkSchemaVersion refuses a database a newer sqi-server migrated.
func checkSchemaVersion(ctx context.Context, db *sql.DB) error {
	v, err := SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if err := migrations.CheckVersion("postgres", v); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	return nil
}

// migrateUp applies every pending migration.
func migrateUp(ctx context.Context, db *sql.DB) error {
	p, err := migrations.NewProvider(db, "postgres")
	if err != nil {
		return fmt.Errorf("postgres: migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("postgres: migrate up: %w", err)
	}
	return nil
}

// sharedConfig returns the dialect and options the shared SQL store is built
// with.
func sharedConfig(opts Options, log *slog.Logger) (sqlstore.Dialect, []sqlstore.Option) {
	var d sqlstore.Dialect = pgDialect{}
	if opts.WrapDialect != nil {
		d = opts.WrapDialect(d)
	}
	sopts := []sqlstore.Option{sqlstore.WithLogger(log), sqlstore.WithTaskInsertBatch(taskInsertBatchRows)}
	if opts.ForceTxRetry {
		sopts = append(sopts, sqlstore.WithForcedRetry())
	}
	return d, sopts
}

// openPool opens a database/sql pool over pgx with READ COMMITTED pinned.
func openPool(dsn string, maxConns int) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_isolation"] = "read committed"
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = "sqi-server"
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	// pgx prepares each distinct statement text as a named server-side
	// statement and keeps up to 512 per connection, so a connection that lived
	// as long as the server would hold every statement it ever ran. Recycling
	// connections bounds that, and the memory of the PostgreSQL backend process
	// behind each one. database/sql recycles only idle connections in the
	// pool, so the server lock's *sql.Conn, checked out for the store's life,
	// is never closed under the lock it holds.
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)
	return db, nil
}

// connMaxLifetime and connMaxIdleTime bound how long a pooled connection
// lives, and sits unused, before database/sql closes it (see openPool).
const (
	connMaxLifetime = time.Hour
	connMaxIdleTime = 10 * time.Minute
)

// Ping checks the read pool, so a burst of writers holding every write
// connection does not fail readiness.
func (s *Store) Ping(ctx context.Context) error { return s.rdb.PingContext(ctx) }

// DSNForTest returns the DSN the store was opened with. Tests open raw
// connections to the same database with it.
func (s *Store) DSNForTest() string { return s.dsn }

// Close releases the server lock, then closes every prepared statement and
// both pools (sqlstore.Store.Close owns the pools from Open's success on).
// Safe to call more than once; later calls return nil.
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.releaseLock(), s.Store.Close())
}

// releaseLock releases the server lock, if held. Safe to call more than once.
func (s *Store) releaseLock() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.release()
	s.lock = nil
	return err
}

// closePools undoes a failed Open: it releases the server lock, if taken, and
// closes whichever pools Open still owns.
func (s *Store) closePools() error {
	errs := []error{s.releaseLock()}
	if s.rdb != nil {
		errs = append(errs, s.rdb.Close())
	}
	if s.db != nil {
		errs = append(errs, s.db.Close())
	}
	return errors.Join(errs...)
}

// SchemaVersion returns the highest applied goose version, or 0 when the
// database has never been migrated. It creates nothing.
func SchemaVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var present bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&present); err != nil {
		return 0, fmt.Errorf("postgres: read schema version: %w", err)
	}
	if !present {
		return 0, nil
	}
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
		return 0, fmt.Errorf("postgres: read schema version: %w", err)
	}
	return v.Int64, nil
}
