// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ServerLockKey is the advisory lock a running sqi-server holds on its
// database (the bytes spell "sqi-srvr"). Advisory locks are per database, so
// servers on different databases of one cluster do not collide.
const ServerLockKey int64 = 0x7371692d73727672

// ErrServerLockHeld means another sqi-server, or a command that needs the
// database alone, already holds it. Servers share a database only when every
// one of them runs with high availability enabled.
var ErrServerLockHeld = errors.New("another sqi-server, or a command that needs the database alone " +
	"(migrate up, restore, store copy), is already using this database " +
	"(PostgreSQL advisory lock 0x7371692d73727672 is held); stop it first. " +
	"Servers can share a database only when high availability is enabled on all of them")

// ErrSchemaBehindWithOtherServers means the database needs migrating, which
// only a server that has the database to itself may do.
var ErrSchemaBehindWithOtherServers = errors.New("the database schema is behind this sqi-server")

// haLockWait is how long an HA server keeps trying for the shared lock while
// another session holds it exclusive: a server that is migrating, or a command
// about to finish. haLockPoll is the pause between tries.
var haLockWait = 30 * time.Second

const haLockPoll = 250 * time.Millisecond

// serverLock holds the advisory lock on a connection taken out of the pool
// for the store's life, and watches that connection.
type serverLock struct {
	conn *sql.Conn
	stop chan struct{}
	done chan struct{}

	ha        bool // taken for a server in an HA group (see acquireHAServerLock)
	exclusive bool // the session holds the lock exclusive
	shared    bool // the session holds the lock shared
}

func acquireServerLock(ctx context.Context, db *sql.DB, log *slog.Logger) (*serverLock, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: server lock connection: %w", err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, ServerLockKey).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("postgres: take server lock: %w", err)
	}
	if !got {
		_ = conn.Close()
		return nil, ErrServerLockHeld
	}
	return startLock(ctx, conn, &serverLock{exclusive: true}, log), nil
}

// startLock finishes l for conn and starts the watcher on it.
func startLock(ctx context.Context, conn *sql.Conn, l *serverLock, log *slog.Logger) *serverLock {
	l.conn, l.stop, l.done = conn, make(chan struct{}), make(chan struct{})
	// The watcher outlives Open, so it must not inherit ctx's cancellation.
	go l.watch(context.WithoutCancel(ctx), log)
	return l
}

// acquireHAServerLock takes the lock for a server in an HA group. It tries
// exclusive first: if that is granted no other server is running, and the
// caller migrates before calling [serverLock.downgrade] to the shared lock the
// group's servers hold together. If it is not granted, others hold it shared
// (or one holds it exclusive, to migrate or as a command), so it asks for it
// shared, retrying for haLockWait, and gives up with ErrServerLockHeld.
func acquireHAServerLock(ctx context.Context, db *sql.DB, log *slog.Logger) (*serverLock, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: server lock connection: %w", err)
	}
	fail := func(err error) (*serverLock, error) {
		return nil, errors.Join(err, conn.Close())
	}
	got, err := tryLock(ctx, conn, "pg_try_advisory_lock")
	if err != nil {
		return fail(err)
	}
	if got {
		return startLock(ctx, conn, &serverLock{ha: true, exclusive: true}, log), nil
	}
	deadline := time.Now().Add(haLockWait)
	for {
		if got, err = tryLock(ctx, conn, "pg_try_advisory_lock_shared"); err != nil {
			return fail(err)
		}
		if got {
			return startLock(ctx, conn, &serverLock{ha: true, shared: true}, log), nil
		}
		if !time.Now().Before(deadline) {
			return fail(ErrServerLockHeld)
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(haLockPoll):
		}
	}
}

// tryLock calls fn (a pg_try_advisory_lock variant) on the server lock's key.
func tryLock(ctx context.Context, conn *sql.Conn, fn string) (bool, error) {
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT `+fn+`($1)`, ServerLockKey).Scan(&got); err != nil {
		return false, fmt.Errorf("postgres: take server lock: %w", err)
	}
	return got, nil
}

// downgrade swaps the exclusive lock for the shared one. The session takes the
// shared lock before it drops the exclusive one, so no other session can
// take the database in between.
func (l *serverLock) downgrade(ctx context.Context) error {
	got, err := tryLock(ctx, l.conn, "pg_try_advisory_lock_shared")
	if err != nil {
		return err
	}
	if !got { // a session never conflicts with its own lock
		return errors.New("postgres: take server lock shared: refused while holding it exclusive")
	}
	l.shared = true
	if _, err := l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, ServerLockKey); err != nil {
		return fmt.Errorf("postgres: drop exclusive server lock: %w", err)
	}
	l.exclusive = false
	return nil
}

// otherHolders counts the other sessions holding the lock shared.
func (l *serverLock) otherHolders(ctx context.Context) (int, error) {
	var n int
	err := l.conn.QueryRowContext(ctx, `
SELECT count(*) FROM pg_locks
WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid AND objsubid = 1
  AND granted AND pid <> pg_backend_pid()`, ServerLockKey).Scan(&n)
	return n, err
}

// watch pings the lock's connection every 30 s. A failed ping means the
// session, and with it the lock, is gone: it is logged, not fatal. Exiting
// would turn a network blip into an outage, and the server does not fail over.
func (l *serverLock) watch(ctx context.Context, log *slog.Logger) {
	defer close(l.done)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := l.conn.PingContext(pctx)
			cancel()
			if err != nil {
				log.ErrorContext(ctx, "store: lost the PostgreSQL server lock's connection; another sqi-server could now start on this database",
					slog.Any("error", err))
				return
			}
		}
	}
}

// releaseTimeout bounds the unlock at shutdown. The lock dies with its session
// anyway, so a connection that no longer answers (a dead network path) must
// not hold the shutdown up.
const releaseTimeout = 5 * time.Second

func (l *serverLock) release() error {
	close(l.stop)
	<-l.done
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	var errs []error
	if l.exclusive {
		_, err := l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, ServerLockKey)
		errs = append(errs, err)
	}
	if l.shared {
		_, err := l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock_shared($1)`, ServerLockKey)
		errs = append(errs, err)
	}
	return errors.Join(append(errs, l.conn.Close())...)
}

// DatabaseLock is the single-server lock held by a command (restore, store
// copy, migrate up) rather than by a store: its own one-connection pool.
type DatabaseLock struct {
	db   *sql.DB
	lock *serverLock
}

// LockDatabase takes the server lock on the database dsn names, failing with
// ErrServerLockHeld when a server (or another such command) holds it.
func LockDatabase(ctx context.Context, dsn string, log *slog.Logger) (*DatabaseLock, error) {
	if log == nil {
		log = slog.Default()
	}
	db, err := openPool(dsn, 1, nil)
	if err != nil {
		return nil, err
	}
	l, err := acquireServerLock(ctx, db, log)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &DatabaseLock{db: db, lock: l}, nil
}

// Release unlocks and closes the lock's connection.
func (d *DatabaseLock) Release() error {
	return errors.Join(d.lock.release(), d.db.Close())
}
