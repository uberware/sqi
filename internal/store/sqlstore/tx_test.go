// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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

// TestInTx_RetriesUpToTheBound pins the retry bound (five attempts) and the
// retry counter: four re-runs after a function that never stops failing, two
// after one that fails twice and then succeeds.
func TestInTx_RetriesUpToTheBound(t *testing.T) {
	retryable := errors.New("serialization failure")
	base := openTestStoreWB(t)
	s := withDialect(t, base, retryDialect{Dialect: base.Dialect(), retryable: retryable})

	calls := 0
	err := s.InTx(t.Context(), "always retryable", func(*sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		calls++
		return sqlstore.TxRollback, retryable
	})
	if !errors.Is(err, retryable) {
		t.Fatalf("err = %v, want the retryable error after the last attempt", err)
	}
	if calls != sqlstore.MaxTxAttempts || sqlstore.MaxTxAttempts != 5 {
		t.Fatalf("calls = %d, MaxTxAttempts = %d; want 5 and 5", calls, sqlstore.MaxTxAttempts)
	}
	if got := s.TxRetries(); got != 4 {
		t.Fatalf("TxRetries = %d, want 4", got)
	}

	calls = 0
	err = s.InTx(t.Context(), "retryable twice", func(*sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		calls++
		if calls < 3 {
			return sqlstore.TxRollback, retryable
		}
		return sqlstore.TxCommit, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err = %v, calls = %d; want nil and 3", err, calls)
	}
	if got := s.TxRetries(); got != 6 {
		t.Fatalf("TxRetries = %d, want 6 (4 + 2)", got)
	}
}

// TestInTx_NoRetryForOtherErrors checks that an error the dialect does not
// classify as Retry runs fn once and leaves the counter alone.
func TestInTx_NoRetryForOtherErrors(t *testing.T) {
	base := openTestStoreWB(t)
	s := withDialect(t, base, retryDialect{Dialect: base.Dialect(), retryable: errors.New("serialization failure")})
	boom := errors.New("boom")
	calls := 0
	err := s.InTx(t.Context(), "boom", func(*sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		calls++
		return sqlstore.TxRollback, boom
	})
	if !errors.Is(err, boom) || calls != 1 || s.TxRetries() != 0 {
		t.Fatalf("err = %v, calls = %d, TxRetries = %d; want boom, 1, 0", err, calls, s.TxRetries())
	}
}

// TestInTx_RetryBackoffStopsWithTheContext checks that a context that ends
// during the pause before a retry ends the transaction with the context's
// error instead of running the remaining attempts.
func TestInTx_RetryBackoffStopsWithTheContext(t *testing.T) {
	retryable := errors.New("serialization failure")
	base := openTestStoreWB(t)
	s := withDialect(t, base, retryDialect{Dialect: base.Dialect(), retryable: retryable})
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	err := s.InTx(ctx, "canceled", func(*sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		calls++
		cancel()
		return sqlstore.TxRollback, retryable
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want context.Canceled after 1 call", err, calls)
	}
}

// TestInTx_ForcedRetryRunsEveryFunctionTwice checks the test mode that runs
// each function twice to exercise re-running it: the first run's writes are
// rolled back and the second is the real one.
func TestInTx_ForcedRetryRunsEveryFunctionTwice(t *testing.T) {
	base := openTestStoreWB(t)
	s := withDialect(t, base, base.Dialect(), sqlstore.WithForcedRetry())
	calls := 0
	if err := s.InTx(t.Context(), "op", func(tx *sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		calls++
		_, err := tx.ExecContext(t.Context(), insertTxFarm, "f1", "one")
		return sqlstore.TxCommit, err
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one rolled-back run, then the real one)", calls)
	}
	if n := farmCount(t, s); n != 1 {
		t.Fatalf("farms = %d, want 1: the rehearsal's insert must be rolled back, or the real run's would fail", n)
	}
}

// TestInTx_ForcedRetryIgnoresTheRehearsalsError checks that an error from the
// rehearsal run does not end the transaction: only the real run's outcome
// counts.
func TestInTx_ForcedRetryIgnoresTheRehearsalsError(t *testing.T) {
	base := openTestStoreWB(t)
	s := withDialect(t, base, base.Dialect(), sqlstore.WithForcedRetry())
	calls := 0
	err := s.InTx(t.Context(), "op", func(*sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		calls++
		if calls == 1 {
			return sqlstore.TxRollback, errors.New("only the rehearsal fails")
		}
		return sqlstore.TxCommit, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err = %v, calls = %d; want nil and 2", err, calls)
	}
	if got := s.TxRetries(); got != 0 {
		t.Fatalf("TxRetries = %d, want 0: a rehearsal is not a retry", got)
	}
}

// TestInTx_RetryDiagnosticsGoToTheLogger checks WithLogger: each re-run logs
// one record naming the operation and the attempt.
func TestInTx_RetryDiagnosticsGoToTheLogger(t *testing.T) {
	retryable := errors.New("serialization failure")
	base := openTestStoreWB(t)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := withDialect(t, base, retryDialect{Dialect: base.Dialect(), retryable: retryable}, sqlstore.WithLogger(log))
	calls := 0
	if err := s.InTx(t.Context(), "lease task", func(*sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		calls++
		if calls == 1 {
			return sqlstore.TxRollback, retryable
		}
		return sqlstore.TxCommit, nil
	}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"store: retrying transaction", `op="lease task"`, "attempt=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output %q lacks %q", out, want)
		}
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
