// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
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

// leasePoolMaxConns is the lease pool's size when Options.HA is set: one
// connection for a renewal, one beside it for the membership upsert, so
// neither waits for the other.
const leasePoolMaxConns = 2

// DefaultLeaseTTL is Options.LeaseTTL when it is zero: ha.lease_ttl's default.
const DefaultLeaseTTL = 15 * time.Second

// Options configures [Open].
type Options struct {
	// MaxConns is the write pool's size (store.postgres.max_conns).
	MaxConns int
	// AutoMigrate runs pending migrations before Open returns.
	AutoMigrate bool
	// ServerLock takes the advisory lock on the database for the store's life
	// (see ServerLockKey). The server sets it; the CLIs do not. The lock's
	// connection comes out of the write pool, so a server with MaxConns 16 has
	// 15 for its work, and ServerLock needs MaxConns of at least 2. It is
	// exclusive, except with HA, when servers of one group hold it shared.
	ServerLock bool
	// HA opens a pool of its own for the leader lease and the server
	// membership (see leasePoolMaxConns), outside the write pool, so a lease
	// renewal never waits behind other writes however many hold its
	// connections. Set it for a server that runs in a group; without it the
	// lease methods share the write pool. HA also bounds every session (see
	// LeaseTTL), and with ServerLock makes the lock shared: the first server
	// to start migrates, and one that joins others requires a current schema.
	HA bool
	// LeaseTTL is the leader lease's lifetime, from which an HA session's
	// idle_in_transaction_session_timeout (a third of it) derives, so a
	// frozen leader's open transactions end before a standby takes over.
	// DefaultLeaseTTL when zero. Ignored without HA.
	LeaseTTL time.Duration
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
	leaseDB *sql.DB     // the HA lease pool, owned by the sqlstore; nil without HA
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
	session := sessionParams(opts)
	db, err := openPool(dsn, opts.MaxConns, session)
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
		if s.lock, err = takeServerLock(ctx, db, opts, log); err != nil {
			return fail(err)
		}
	}
	if err := checkSchemaVersion(ctx, db); err != nil {
		return fail(err)
	}
	if err := s.prepareSchema(ctx, opts); err != nil {
		return fail(err)
	}
	if s.rdb, err = openPool(dsn, readPoolMaxConns, session); err != nil {
		return fail(err)
	}
	if s.Store, s.leaseDB, err = newSQLStore(ctx, dsn, db, s.rdb, opts, log); err != nil {
		// New closed every pool itself; only the lock is left to release.
		s.db, s.rdb = nil, nil
		return fail(err)
	}
	return s, nil
}

// takeServerLock takes the database lock the way opts says: exclusive, or for
// a server in an HA group the shared lock described at acquireHAServerLock.
func takeServerLock(ctx context.Context, db *sql.DB, opts Options, log *slog.Logger) (*serverLock, error) {
	if opts.HA {
		return acquireHAServerLock(ctx, db, log)
	}
	return acquireServerLock(ctx, db, log)
}

// prepareSchema brings the schema up to date as opts says. Only a server alone
// on the database migrates: one that joined others (an HA server whose lock
// is shared from the start) requires the schema to be current instead, and a
// server that migrated under the exclusive lock then drops to the shared one.
func (s *Store) prepareSchema(ctx context.Context, opts Options) error {
	if s.lock != nil && s.lock.ha && !s.lock.exclusive {
		if !opts.AutoMigrate {
			return nil
		}
		return s.requireCurrentSchema(ctx)
	}
	if opts.AutoMigrate {
		if err := migrateUp(ctx, s.db); err != nil {
			return err
		}
	}
	if s.lock != nil && s.lock.ha {
		return s.lock.downgrade(ctx)
	}
	return nil
}

// requireCurrentSchema refuses a database that has migrations this binary
// would apply, naming the servers that stop the migration.
func (s *Store) requireCurrentSchema(ctx context.Context) error {
	have, err := SchemaVersion(ctx, s.db)
	if err != nil {
		return err
	}
	want, err := migrations.MaxVersion("postgres")
	if err != nil {
		return err
	}
	if have >= want {
		return nil
	}
	others, err := s.lock.otherHolders(ctx)
	if err != nil || others < 1 {
		others = 1 // we got the shared lock only because someone holds the database
	}
	return fmt.Errorf("postgres: schema is at version %d and this sqi-server needs %d, but %d other sqi-server(s) are using this database; "+
		"stop them all to migrate, or run `sqi-server migrate up` once they are stopped: %w", have, want, others, ErrSchemaBehindWithOtherServers)
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

// newSQLStore builds the shared SQL store over the write and read pools. With
// opts.HA it also opens the dedicated lease pool, which the store owns from
// then on (New closes every pool it was given, this one included, when it
// fails) and returns it for the tests to look at.
func newSQLStore(ctx context.Context, dsn string, write, read *sql.DB, opts Options, log *slog.Logger) (*sqlstore.Store, *sql.DB, error) {
	d, sopts := sharedConfig(opts, log)
	var leaseDB *sql.DB
	if opts.HA {
		var err error
		if leaseDB, err = openPool(dsn, leasePoolMaxConns, sessionParams(opts)); err != nil {
			return nil, nil, errors.Join(err, write.Close(), read.Close())
		}
		sopts = append(sopts, sqlstore.WithLeasePool(leaseDB))
	}
	st, err := sqlstore.New(ctx, write, read, d, sopts...)
	if err != nil {
		return nil, nil, err
	}
	return st, leaseDB, nil
}

// sessionParams returns the extra session settings every connection of an HA
// store carries; nil without HA.
//
// A frozen leader (a stopped process, a paused VM) still answers TCP
// keepalives, so PostgreSQL never notices it, and whatever rows its open
// transaction holds stay locked. idle_in_transaction_session_timeout, a third
// of the lease, has PostgreSQL end such a session; the lease has by then
// expired and a standby is taking over. client_connection_check_interval
// cancels a long statement whose client has gone away.
func sessionParams(opts Options) map[string]string {
	if !opts.HA {
		return nil
	}
	ttl := opts.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	return map[string]string{
		"idle_in_transaction_session_timeout": strconv.FormatInt(max(1, (ttl/3).Milliseconds()), 10),
		"client_connection_check_interval":    "1000",
	}
}

// openPool opens a database/sql pool over pgx with READ COMMITTED pinned and
// extra (see sessionParams) set on every connection.
func openPool(dsn string, maxConns int, extra map[string]string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_isolation"] = "read committed"
	maps.Copy(cfg.RuntimeParams, extra)
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
// every pool (sqlstore.Store.Close owns the pools from Open's success on).
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

// SchemaVersionDSN is [SchemaVersion] for the database dsn names, on a
// connection of its own.
func SchemaVersionDSN(ctx context.Context, dsn string) (int64, error) {
	db, err := openPool(dsn, 1, nil)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return SchemaVersion(ctx, db)
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
