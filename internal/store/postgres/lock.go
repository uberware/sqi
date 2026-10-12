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

// ErrServerLockHeld means another sqi-server already holds the database.
var ErrServerLockHeld = errors.New("another sqi-server is already using this database " +
	"(PostgreSQL advisory lock 0x7371692d73727672 is held); running more than one server on one database is not supported yet")

// serverLock holds the advisory lock on a connection taken out of the pool
// for the store's life, and watches that connection.
type serverLock struct {
	conn *sql.Conn
	stop chan struct{}
	done chan struct{}
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
	l := &serverLock{conn: conn, stop: make(chan struct{}), done: make(chan struct{})}
	// The watcher outlives Open, so it must not inherit ctx's cancellation.
	go l.watch(context.WithoutCancel(ctx), log)
	return l, nil
}

// watch pings the lock's connection every 30 s. A failed ping means the
// session, and with it the lock, is gone: it is logged, not fatal. Exiting
// would turn a network blip into an outage, and failover is H7's.
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

func (l *serverLock) release() error {
	close(l.stop)
	<-l.done
	_, err := l.conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, ServerLockKey)
	return errors.Join(err, l.conn.Close())
}
