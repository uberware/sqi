// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uberware/sqi/internal/store"
)

// The write fence. A leader runs its singleton work under a leadership
// context ([store.WithLeaderEpoch]); every write made under one commits only
// while that epoch is still the database's, so a server that has lost the
// lead (paused, partitioned, frozen mid-transaction) cannot commit anything
// once another has taken over. The check is the last statement before the
// commit ([Store.checkFence]), so the epoch row is held only from the check to
// the commit, however long the transaction ran before it, and a takeover's
// UPDATE of that row waits only for commits already past their check.

// sqlCheckFence finds the epoch row only while it still holds the leader's
// epoch. On PostgreSQL it is run with FOR SHARE (see [Store.checkFence]);
// [New] appends the dialect's clause once, as Store.fenceCheckSQL.
const sqlCheckFence = `SELECT 1 FROM server_epoch WHERE id = 1 AND epoch = ?`

// unfencedKey marks a context whose writes are never fenced ([unfenced]).
type unfencedKey struct{}

// unfenced returns ctx with any leader epoch it carries ignored by the fence.
// It exists for [Store.AcquireLeadership], the one leadership method that
// runs a transaction ([Store.inTxOn], which checks the fence at commit): a
// server taking the lead must not be fenced by the epoch it is replacing. The
// other leadership methods issue their statements on the lease pool directly
// and never reach the fence.
func unfenced(ctx context.Context) context.Context {
	return context.WithValue(ctx, unfencedKey{}, true)
}

// fenceEpoch returns the epoch a write under ctx is fenced by, and whether it
// is fenced at all. A context with no epoch, the default farm's every write,
// costs one lookup.
func fenceEpoch(ctx context.Context) (int64, bool) {
	epoch, ok := store.LeaderEpoch(ctx)
	if !ok || ctx.Value(unfencedKey{}) != nil {
		return 0, false
	}
	return epoch, true
}

// fenced reports whether a write under ctx is fenced.
func fenced(ctx context.Context) bool {
	_, ok := fenceEpoch(ctx)
	return ok
}

// checkFence runs the fence check in tx when ctx carries a leader epoch: the
// epoch row read FOR SHARE (on a database with row locks) where it still holds
// that epoch. No row means another server has taken over, and the caller
// rolls back with [store.ErrFenced]. Without an epoch it does nothing.
func (s *Store) checkFence(ctx context.Context, tx *dbTx, op string) error {
	epoch, ok := fenceEpoch(ctx)
	if !ok {
		return nil
	}
	var one int
	err := tx.QueryRowContext(ctx, s.fenceCheckSQL, epoch).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: %s at epoch %d: %w", op, epoch, store.ErrFenced)
	}
	if err != nil {
		return fmt.Errorf("store: fence check for %s: %w", op, mapErr(err))
	}
	return nil
}

// writer runs a write's statements: the write pool itself, or a transaction
// on it. [*dbPool] and [*dbTx] both satisfy it. Stmt's result may be closed
// either way: on a transaction that closes the transaction's copy, on the pool
// it does nothing, the statement being the store's own.
type writer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *dbRow
	Stmt(ctx context.Context, stmt *dbStmt) *dbStmt
}

// write runs fn, a write that needs no transaction of its own, so that a
// leadership context fences it. Without a leader epoch on ctx fn runs on the
// write pool itself and issues exactly its own statements. With one fn runs in
// a transaction ([Store.writeTx]). op names the operation in errors.
func (s *Store) write(ctx context.Context, op string, fn func(w writer) error) error {
	if !fenced(ctx) {
		return fn(s.w)
	}
	return s.writeTx(ctx, op, fn)
}

// writeTx runs fn in a transaction ([Store.inTx], retried as any other),
// committed when it returns nil after the fence check, and rolled back when it
// returns an error: [Store.write]'s path under a leadership context. fn must
// finish reading any rows it queries before it returns, and must not depend on
// state left by an earlier run.
func (s *Store) writeTx(ctx context.Context, op string, fn func(w writer) error) error {
	return s.inTx(ctx, op, func(tx *dbTx) (txOutcome, error) {
		if err := fn(tx); err != nil {
			return txRollback, err
		}
		return txCommit, nil
	})
}

// execStmt runs stmt, a write, as [Store.write] would and returns its result.
// Without a leader epoch it is stmt run on the write pool, where Stmt would
// hand back stmt itself.
func (s *Store) execStmt(ctx context.Context, op string, stmt *dbStmt, args ...any) (sql.Result, error) {
	if !fenced(ctx) {
		return stmt.ExecContext(ctx, args...)
	}
	var res sql.Result
	err := s.writeTx(ctx, op, func(w writer) (err error) {
		res, err = w.Stmt(ctx, stmt).ExecContext(ctx, args...)
		return err
	})
	return res, err
}

// exec runs query, a write, as [Store.write] would and returns its result.
func (s *Store) exec(ctx context.Context, op, query string, args ...any) (sql.Result, error) {
	if !fenced(ctx) {
		return s.w.ExecContext(ctx, query, args...)
	}
	var res sql.Result
	err := s.writeTx(ctx, op, func(w writer) (err error) {
		res, err = w.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

// writeRow runs stmt, a write that returns one row (… RETURNING), as
// [Store.write] would and returns the row as scan reads it.
func writeRow[T any](ctx context.Context, s *Store, op string, scan func(scanner) (T, error), stmt *dbStmt, args ...any) (T, error) {
	if !fenced(ctx) {
		return scan(stmt.QueryRowContext(ctx, args...))
	}
	var out T
	err := s.writeTx(ctx, op, func(w writer) (err error) {
		out, err = scan(w.Stmt(ctx, stmt).QueryRowContext(ctx, args...))
		return err
	})
	return out, err
}
