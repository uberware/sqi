// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"fmt"
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
// database aborted for a concurrent writer (ErrClassRetry). SQLite's single
// write connection never produces one.
const maxTxAttempts = 3

// inTx runs fn in a transaction on the write pool and commits it when fn
// returns txCommit and no error. On an error the dialect classifies as
// ErrClassRetry (a class a wrapper attached, or one the dialect gives fn's own
// error) it runs fn again, up to maxTxAttempts times, so fn must not depend on
// state left by a failed run. op names the operation in errors.
func (s *Store) inTx(ctx context.Context, op string, fn func(tx *dbTx) (txOutcome, error)) error {
	for attempt := 1; ; attempt++ {
		err := s.runTx(ctx, op, fn)
		if err == nil || attempt == maxTxAttempts || errClass(classifyErr(s.d, err)) != ErrClassRetry {
			return err
		}
	}
}

func (s *Store) runTx(ctx context.Context, op string, fn func(tx *dbTx) (txOutcome, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin %s: %w", op, mapErr(classifyErr(s.d, err)))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	outcome, err := fn(&dbTx{tx: tx, d: s.d})
	if err != nil || outcome == txRollback {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit %s: %w", op, mapErr(classifyErr(s.d, err)))
	}
	return nil
}
