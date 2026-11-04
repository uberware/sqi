// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

// queryLog wraps a dialect and records the text of every statement rebound
// through it, after which it hands the statement to the dialect it wraps.
type queryLog struct {
	sqlstore.Dialect

	mu      *sync.Mutex
	queries *[]string
}

func newQueryLog(d sqlstore.Dialect) queryLog {
	return queryLog{Dialect: d, mu: new(sync.Mutex), queries: new([]string)}
}

func (l queryLog) Rebind(q string) string {
	l.mu.Lock()
	*l.queries = append(*l.queries, q)
	l.mu.Unlock()
	return l.Dialect.Rebind(q)
}

// reset forgets what was recorded so far (statement preparation, seeding).
func (l queryLog) reset() {
	l.mu.Lock()
	*l.queries = nil
	l.mu.Unlock()
}

func (l queryLog) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(*l.queries)
}

// retryAll wraps a dialect and classifies every error as retryable, so a test
// can show an error is never retried whatever the dialect says of it.
type retryAll struct{ sqlstore.Dialect }

func (retryAll) Classify(error) sqlstore.ErrClass { return sqlstore.ErrClassRetry }

// fenceStore is a store for the fence tests and the log of its statements.
type fenceStore struct {
	s   *sqlstore.Store
	log queryLog
}

// fenceStores returns a SQLite store and, when PostgreSQL is configured, a
// PostgreSQL one, each with its statements recorded (and its dialect wrapped
// by wrap, when not nil).
func fenceStores(t *testing.T, wrap func(sqlstore.Dialect) sqlstore.Dialect) map[string]fenceStore {
	t.Helper()
	build := func(d sqlstore.Dialect) (sqlstore.Dialect, queryLog) {
		if wrap != nil {
			d = wrap(d)
		}
		l := newQueryLog(d)
		return l, l
	}
	base := openTestStoreWB(t)
	d, l := build(base.Dialect())
	out := map[string]fenceStore{"sqlite": {s: withDialect(t, base, d), log: l}}
	if storetest.PostgresEnabled() {
		var pl queryLog
		opts := postgres.DefaultOptions()
		opts.WrapDialect = func(d sqlstore.Dialect) sqlstore.Dialect {
			var w sqlstore.Dialect
			w, pl = build(d)
			return w
		}
		out["postgres"] = fenceStore{s: storetest.PostgresWith(t, opts).Store, log: pl}
	}
	return out
}

// setFenceEpoch moves the database's fencing epoch to epoch, as a takeover
// would.
func setFenceEpoch(t *testing.T, s *sqlstore.Store, epoch int64) {
	t.Helper()
	if _, err := s.WritePool().ExecContext(t.Context(), `UPDATE server_epoch SET epoch = ? WHERE id = 1`, epoch); err != nil {
		t.Fatalf("set server_epoch: %v", err)
	}
}

func mentionsFence(q string) bool { return strings.Contains(q, "server_epoch") }

// insertFenceFarm writes one farm, stamped as PostgreSQL's ts domain wants.
const insertFenceFarm = `INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`

func insertFenceFarmArgs(id string) []any {
	now := sqlstore.TimeText(time.Now().UTC())
	return []any{id, id, now, now}
}

// insertFarm is a transaction body that writes one farm.
func insertFarm(ctx context.Context, id string) func(tx *sqlstore.DBTx) (sqlstore.TxOutcome, error) {
	return func(tx *sqlstore.DBTx) (sqlstore.TxOutcome, error) {
		_, err := tx.ExecContext(ctx, insertFenceFarm, insertFenceFarmArgs(id)...)
		return sqlstore.TxCommit, err
	}
}

// TestRunTx_FenceCheckedLastBeforeCommit: under a leadership context the
// epoch check is the transaction's last statement. A current epoch commits;
// a stale one rolls fn's writes back with ErrFenced and is not run again.
func TestRunTx_FenceCheckedLastBeforeCommit(t *testing.T) {
	for name, fs := range fenceStores(t, nil) {
		t.Run(name, func(t *testing.T) {
			s := fs.s
			setFenceEpoch(t, s, 2)

			fs.log.reset()
			current := store.WithLeaderEpoch(t.Context(), 2)
			if err := s.InTx(current, "test", insertFarm(current, "f-current")); err != nil {
				t.Fatalf("current epoch: %v", err)
			}
			got := fs.log.recorded()
			if len(got) < 2 || !mentionsFence(got[len(got)-1]) || slices.ContainsFunc(got[:len(got)-1], mentionsFence) {
				t.Fatalf("statements = %q, want fn's then the fence check, once, last", got)
			}
			// The row lock is what makes a takeover wait for a commit past its check.
			if check := got[len(got)-1]; !strings.HasSuffix(check, s.Dialect().ForShare()) {
				t.Fatalf("fence check %q does not end with the dialect's %q", check, s.Dialect().ForShare())
			}
			if farmCount(t, s) != 1 {
				t.Fatalf("farms = %d after a current-epoch commit, want 1", farmCount(t, s))
			}

			stale := store.WithLeaderEpoch(t.Context(), 1)
			err := s.InTx(stale, "test", insertFarm(stale, "f-stale"))
			if !errors.Is(err, store.ErrFenced) {
				t.Fatalf("stale epoch: err = %v, want ErrFenced", err)
			}
			if farmCount(t, s) != 1 {
				t.Fatalf("farms = %d after a fenced transaction, want its insert rolled back", farmCount(t, s))
			}
			if n := s.TxRetries(); n != 0 {
				t.Fatalf("TxRetries = %d, want a fenced transaction never re-run", n)
			}
		})
	}
}

// TestRunTx_FencedIsNeverRetried: ErrFenced is final even under a dialect
// that would call any error retryable.
func TestRunTx_FencedIsNeverRetried(t *testing.T) {
	wrap := func(d sqlstore.Dialect) sqlstore.Dialect { return retryAll{d} }
	for name, fs := range fenceStores(t, wrap) {
		t.Run(name, func(t *testing.T) {
			s := fs.s
			setFenceEpoch(t, s, 2)
			runs := 0
			stale := store.WithLeaderEpoch(t.Context(), 1)
			err := s.InTx(stale, "test", func(*sqlstore.DBTx) (sqlstore.TxOutcome, error) {
				runs++
				return sqlstore.TxCommit, nil
			})
			if !errors.Is(err, store.ErrFenced) {
				t.Fatalf("err = %v, want ErrFenced", err)
			}
			// A store built with forced retry rehearses fn once, uncounted.
			if n := s.TxRetries(); n != 0 || runs > 2 {
				t.Fatalf("TxRetries = %d after %d runs, want ErrFenced never retried", n, runs)
			}
		})
	}
}

// TestRunTx_NoEpochNoFenceStatement: a transaction under a plain context
// issues no fence statement and commits whatever the epoch.
func TestRunTx_NoEpochNoFenceStatement(t *testing.T) {
	for name, fs := range fenceStores(t, nil) {
		t.Run(name, func(t *testing.T) {
			s := fs.s
			setFenceEpoch(t, s, 7)
			fs.log.reset()
			ctx := t.Context()
			if err := s.InTx(ctx, "test", insertFarm(ctx, "f1")); err != nil {
				t.Fatalf("InTx: %v", err)
			}
			if got := fs.log.recorded(); slices.ContainsFunc(got, mentionsFence) {
				t.Fatalf("statements = %q, want no fence check without a leadership context", got)
			}
			if farmCount(t, s) != 1 {
				t.Fatalf("farms = %d, want 1", farmCount(t, s))
			}
		})
	}
}

// TestWrite_NoEpochRunsOnPoolWithoutTx: without a leadership context write
// hands fn the write pool itself, so no transaction is begun and only fn's own
// statements run.
func TestWrite_NoEpochRunsOnPoolWithoutTx(t *testing.T) {
	for name, fs := range fenceStores(t, nil) {
		t.Run(name, func(t *testing.T) {
			s := fs.s
			setFenceEpoch(t, s, 7)
			fs.log.reset()
			ctx := t.Context()
			err := s.Write(ctx, "test", func(w sqlstore.Writer) error {
				if p, ok := w.(*sqlstore.DBPool); !ok || p != s.WritePool() {
					t.Errorf("fn ran on %T, want the store's write pool", w)
				}
				_, err := w.ExecContext(ctx, insertFenceFarm, insertFenceFarmArgs("f1")...)
				return err
			})
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if got := fs.log.recorded(); len(got) != 1 || got[0] != insertFenceFarm {
				t.Fatalf("statements = %q, want fn's insert alone", got)
			}
			if farmCount(t, s) != 1 {
				t.Fatalf("farms = %d, want 1", farmCount(t, s))
			}
		})
	}
}

// TestWrite_EpochRunsInTxWithFence: under a leadership context write runs fn
// in a transaction checked at commit. The current epoch's write lands; a
// stale one's is rolled back with ErrFenced; an error from fn rolls back.
func TestWrite_EpochRunsInTxWithFence(t *testing.T) {
	for name, fs := range fenceStores(t, nil) {
		t.Run(name, func(t *testing.T) {
			s := fs.s
			setFenceEpoch(t, s, 2)
			insert := func(ctx context.Context, id string, after error) error {
				return s.Write(ctx, "test", func(w sqlstore.Writer) error {
					if _, ok := w.(*sqlstore.DBTx); !ok {
						t.Errorf("fn ran on %T, want a transaction", w)
					}
					if _, err := w.ExecContext(ctx, insertFenceFarm, insertFenceFarmArgs(id)...); err != nil {
						return err
					}
					return after
				})
			}

			fs.log.reset()
			current := store.WithLeaderEpoch(t.Context(), 2)
			if err := insert(current, "f-current", nil); err != nil {
				t.Fatalf("current epoch: %v", err)
			}
			if got := fs.log.recorded(); len(got) < 2 || !mentionsFence(got[len(got)-1]) {
				t.Fatalf("statements = %q, want the fence check last", got)
			}
			if farmCount(t, s) != 1 {
				t.Fatalf("farms = %d, want the current leader's insert", farmCount(t, s))
			}

			stale := store.WithLeaderEpoch(t.Context(), 1)
			if err := insert(stale, "f-stale", nil); !errors.Is(err, store.ErrFenced) {
				t.Fatalf("stale epoch: err = %v, want ErrFenced", err)
			}
			boom := errors.New("boom")
			if err := insert(current, "f-boom", boom); !errors.Is(err, boom) {
				t.Fatalf("fn error: err = %v, want boom", err)
			}
			if farmCount(t, s) != 1 {
				t.Fatalf("farms = %d, want the fenced and the failed insert rolled back", farmCount(t, s))
			}
		})
	}
}

// TestWrite_StmtOnPoolIsTheStatement: on the pool, Stmt hands the prepared
// statement back as it is; only a transaction rebinds it.
func TestWrite_StmtOnPoolIsTheStatement(t *testing.T) {
	s := openTestStoreWB(t).Store
	stmt := s.AnyPreparedStmt()
	if got := s.WritePool().Stmt(t.Context(), stmt); got != stmt {
		t.Fatalf("WritePool().Stmt = %p, want the statement itself (%p)", got, stmt)
	}
}

// TestWrite_ClosingTheStatementLeavesTheStoreUsable: a write that closes the
// statement [Writer.Stmt] gave it, as one does with a transaction's, closes a
// copy bound to that transaction on a leadership context, and nothing at all
// without one, where the statement is the store's own and every later call
// shares it.
func TestWrite_ClosingTheStatementLeavesTheStoreUsable(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no epoch": t.Context(),
		"epoch":    store.WithLeaderEpoch(t.Context(), 0),
	} {
		t.Run(name, func(t *testing.T) {
			s := openTestStoreWB(t).Store
			stmt := s.AnyPreparedStmt() // the farm insert, the first statement prepared
			err := s.Write(ctx, "test", func(w sqlstore.Writer) error {
				return w.Stmt(ctx, stmt).Close()
			})
			if err != nil {
				t.Fatalf("closing the statement Write handed out: %v", err)
			}
			for i := range 2 {
				id := fmt.Sprintf("f%d", i)
				if _, err := s.CreateFarm(ctx, store.Farm{ID: id, Name: id}); err != nil {
					t.Fatalf("CreateFarm %d after the close: %v", i, err)
				}
			}
		})
	}
}

// TestMapErr_PassesErrFencedThrough: mapErr neither hides nor reclassifies
// ErrFenced, wrapped or classified.
func TestMapErr_PassesErrFencedThrough(t *testing.T) {
	d := openTestStoreWB(t).Dialect()
	for _, err := range []error{
		store.ErrFenced,
		errors.Join(errors.New("ctx"), store.ErrFenced),
		sqlstore.ClassifyErr(d, store.ErrFenced),
	} {
		got := sqlstore.MapErr(err)
		if !errors.Is(got, store.ErrFenced) || errors.Is(got, store.ErrNotFound) || errors.Is(got, store.ErrConflict) {
			t.Errorf("MapErr(%v) = %v, want ErrFenced unchanged", err, got)
		}
	}
	if c := d.Classify(store.ErrFenced); c == sqlstore.ErrClassRetry {
		t.Fatalf("Classify(ErrFenced) = retry, want never")
	}
}
