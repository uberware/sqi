// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// txOutcome tells inTx whether to commit the transaction fn ran in. A nil
// error does not always mean commit: LeaseTask discards writes that already
// ran when the lease does not land.
type txOutcome int

const (
	txCommit txOutcome = iota
	txRollback
)

// maxTxAttempts bounds how many times inTx runs fn for a transaction the
// database aborted for a concurrent writer (ErrClassRetry), the first run
// included. SQLite's single write connection never produces one; PostgreSQL
// does under contention (a deadlock, a serialization failure).
const maxTxAttempts = 5

// inTx runs fn in a transaction on the write pool and commits it when fn
// returns txCommit and no error. On an error the dialect classifies as
// ErrClassRetry (a class a wrapper attached, or one the dialect gives fn's own
// error) it pauses for a jittered 5–50 ms and runs fn again, up to
// maxTxAttempts runs in all, so fn must not depend on state left by a failed
// run. op names the operation in errors.
//
// A store built [WithForcedRetry] runs every fn once more, first, and rolls
// that run back, so the tests exercise the re-run path on every operation.
func (s *Store) inTx(ctx context.Context, op string, fn func(tx *dbTx) (txOutcome, error)) error {
	if s.forceRetry {
		// The rehearsal's outcome is discarded: its writes are rolled back and
		// its error ignored, exactly as a run the database aborted would be.
		_ = s.runTx(ctx, op, func(tx *dbTx) (txOutcome, error) { //nolint:errcheck // a rehearsal; see above
			_, err := fn(tx)
			return txRollback, err
		})
	}
	for attempt := 1; ; attempt++ {
		err := s.runTx(ctx, op, fn)
		if err == nil || attempt == maxTxAttempts || errClass(classifyErr(s.d, err)) != ErrClassRetry {
			return err
		}
		s.retries.Add(1)
		s.log.DebugContext(ctx, "store: retrying transaction", slog.String("op", op),
			slog.Int("attempt", attempt), slog.Any("error", err))
		if err := sleepCtx(ctx, retryBackoff()); err != nil {
			return err
		}
	}
}

// inTxValue is [Store.inTx] for a transaction that produces a value: fn
// returns it with its outcome. The value returned is the one from the run that
// ended the transaction, committed or rolled back by fn's choice, and the zero
// value with any error. A run the database aborted, a rehearsal, or a commit
// that failed leaves nothing behind, so fn need not reset state from an
// earlier run.
func inTxValue[T any](ctx context.Context, s *Store, op string, fn func(tx *dbTx) (T, txOutcome, error)) (T, error) {
	var out T
	err := s.inTx(ctx, op, func(tx *dbTx) (txOutcome, error) {
		v, outcome, err := fn(tx)
		out = v
		return outcome, err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

func (s *Store) runTx(ctx context.Context, op string, fn func(tx *dbTx) (txOutcome, error)) error {
	tx, err := s.w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin %s: %w", op, mapErr(classifyErr(s.d, err)))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	outcome, err := fn(newDBTx(tx, s.d))
	if err != nil || outcome == txRollback {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit %s: %w", op, mapErr(classifyErr(s.d, err)))
	}
	return nil
}

// retryBackoff returns a jittered pause of 5–50 ms before a retry, so two
// transactions that aborted each other do not collide again in lockstep.
func retryBackoff() time.Duration {
	return 5*time.Millisecond + rand.N(45*time.Millisecond) //nolint:gosec // jitter, not security
}

// sleepCtx sleeps for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
