// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"testing"
)

func openTxStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), t.TempDir()+"/t.db", DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func farmCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM farms`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

const insertTxFarm = `INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, '', '')`

func TestInTx_CommitOutcomeKeepsWrites(t *testing.T) {
	s := openTxStore(t)
	err := s.inTx(t.Context(), "test", func(tx *dbTx) (txOutcome, error) {
		_, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one")
		return txCommit, err
	})
	if err != nil || farmCount(t, s) != 1 {
		t.Fatalf("inTx = %v, farms = %d; want nil, 1", err, farmCount(t, s))
	}
}

// TestInTx_RollbackOutcomeDiscardsWrites pins what LeaseTask relies on: a nil
// error with txRollback discards writes that already ran.
func TestInTx_RollbackOutcomeDiscardsWrites(t *testing.T) {
	s := openTxStore(t)
	err := s.inTx(t.Context(), "test", func(tx *dbTx) (txOutcome, error) {
		_, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one")
		return txRollback, err
	})
	if err != nil || farmCount(t, s) != 0 {
		t.Fatalf("inTx = %v, farms = %d; want nil, 0", err, farmCount(t, s))
	}
}

func TestInTx_ErrorDiscardsWrites(t *testing.T) {
	s := openTxStore(t)
	boom := errors.New("boom")
	err := s.inTx(t.Context(), "test", func(tx *dbTx) (txOutcome, error) {
		if _, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one"); err != nil {
			return txRollback, err
		}
		return txCommit, boom
	})
	if !errors.Is(err, boom) || farmCount(t, s) != 0 {
		t.Fatalf("inTx = %v, farms = %d; want boom, 0", err, farmCount(t, s))
	}
}

// TestInTx_RetriesARetryClassError runs fn again on an error the store
// classifies as retryable, up to maxTxAttempts, and not on any other error.
func TestInTx_RetriesARetryClassError(t *testing.T) {
	retryable := errors.New("serialization failure")
	s := openWithDialect(t, retryDialect{retryable: retryable})
	runs := 0
	err := s.inTx(t.Context(), "test", func(tx *dbTx) (txOutcome, error) {
		runs++
		if runs < 2 {
			return txRollback, retryable
		}
		_, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one")
		return txCommit, err
	})
	if err != nil || runs != 2 || farmCount(t, s) != 1 {
		t.Fatalf("inTx = %v, runs = %d, farms = %d; want nil, 2, 1", err, runs, farmCount(t, s))
	}

	runs = 0
	err = s.inTx(t.Context(), "test", func(*dbTx) (txOutcome, error) {
		runs++
		return txRollback, retryable
	})
	if !errors.Is(err, retryable) || runs != maxTxAttempts {
		t.Fatalf("always retryable: inTx = %v after %d runs; want the error after %d", err, runs, maxTxAttempts)
	}
}

// retryDialect is the SQLite dialect, except that it classifies retryable as
// ErrClassRetry, as a database with concurrent writers classifies a
// serialization failure.
type retryDialect struct {
	sqliteDialect

	retryable error
}

func (d retryDialect) Classify(err error) ErrClass {
	if errors.Is(err, d.retryable) {
		return ErrClassRetry
	}
	return d.sqliteDialect.Classify(err)
}
