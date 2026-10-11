// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

func farmCount(t *testing.T, s *sqlstore.Store) int {
	t.Helper()
	var n int
	if err := s.WriteDB().QueryRowContext(t.Context(), `SELECT count(*) FROM farms`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

const insertTxFarm = `INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, '', '')`

func TestInTx_CommitOutcomeKeepsWrites(t *testing.T) {
	s := openTestStoreWB(t).Store
	err := s.InTx(t.Context(), "test", func(tx *sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		_, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one")
		return sqlstore.TxCommit, err
	})
	if err != nil || farmCount(t, s) != 1 {
		t.Fatalf("inTx = %v, farms = %d; want nil, 1", err, farmCount(t, s))
	}
}

// TestInTx_RollbackOutcomeDiscardsWrites pins what LeaseTask relies on: a nil
// error with txRollback discards writes that already ran.
func TestInTx_RollbackOutcomeDiscardsWrites(t *testing.T) {
	s := openTestStoreWB(t).Store
	err := s.InTx(t.Context(), "test", func(tx *sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		_, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one")
		return sqlstore.TxRollback, err
	})
	if err != nil || farmCount(t, s) != 0 {
		t.Fatalf("inTx = %v, farms = %d; want nil, 0", err, farmCount(t, s))
	}
}

func TestInTx_ErrorDiscardsWrites(t *testing.T) {
	s := openTestStoreWB(t).Store
	boom := errors.New("boom")
	err := s.InTx(t.Context(), "test", func(tx *sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		if _, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one"); err != nil {
			return sqlstore.TxRollback, err
		}
		return sqlstore.TxCommit, boom
	})
	if !errors.Is(err, boom) || farmCount(t, s) != 0 {
		t.Fatalf("inTx = %v, farms = %d; want boom, 0", err, farmCount(t, s))
	}
}

// TestInTx_RetriesARetryClassError checks that inTx runs fn again on an error
// the dialect classifies as retryable, up to maxTxAttempts times in all.
func TestInTx_RetriesARetryClassError(t *testing.T) {
	retryable := errors.New("serialization failure")
	base := openTestStoreWB(t)
	s := withDialect(t, base, retryDialect{Dialect: base.Dialect(), retryable: retryable})
	runs := 0
	err := s.InTx(t.Context(), "test", func(tx *sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		runs++
		if runs < 2 {
			return sqlstore.TxRollback, retryable
		}
		_, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one")
		return sqlstore.TxCommit, err
	})
	if err != nil || runs != 2 || farmCount(t, s) != 1 {
		t.Fatalf("inTx = %v, runs = %d, farms = %d; want nil, 2, 1", err, runs, farmCount(t, s))
	}

	runs = 0
	err = s.InTx(t.Context(), "test", func(*sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		runs++
		return sqlstore.TxRollback, retryable
	})
	if !errors.Is(err, retryable) || runs != sqlstore.MaxTxAttempts {
		t.Fatalf("always retryable: inTx = %v after %d runs; want the error after %d", err, runs, sqlstore.MaxTxAttempts)
	}
}

// retryDialect is the SQLite dialect, except that it classifies retryable as
// ErrClassRetry, as a database with concurrent writers classifies a
// serialization failure.
type retryDialect struct {
	sqlstore.Dialect

	retryable error
}

func (d retryDialect) Classify(err error) sqlstore.ErrClass {
	if errors.Is(err, d.retryable) {
		return sqlstore.ErrClassRetry
	}
	return d.Dialect.Classify(err)
}
