// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

// pause stops the first transaction that reaches a chosen point, until the
// test resumes it. A race test arms it, starts the operation that should be
// caught mid-transaction, runs the competing operation, then resumes: the
// interleaving is forced, so the test goes red without its fix every time,
// not when the scheduler happens to cooperate.
//
// A statement point fires in Rebind, which runs just before a dynamic
// statement executes (prepared statements were rebound at prepare and never
// fire). An anchor point fires in LockAnchors before the lock is taken.
type pause struct {
	mu      sync.Mutex
	armed   bool
	fired   bool
	stmt    string
	table   string
	id      string
	what    string // the point the pause fired at, for matched
	dsn     string // the store's database, which interleave watches for lock waits
	hit     chan struct{}
	release chan struct{}
	once    sync.Once
}

func newPause() *pause { return &pause{hit: make(chan struct{}), release: make(chan struct{})} }

// atStatement arms the pause on the first statement whose text contains sql.
func (p *pause) atStatement(sql string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stmt, p.armed = sql, true
}

// atAnchor arms the pause on the first lock of the row (table, id).
func (p *pause) atAnchor(table, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.table, p.id, p.armed = table, id, true
}

// maybe blocks the caller if the pause is armed and unfired and match reports
// a hit. match also names the point (the query text, or the anchor), which
// matched returns.
func (p *pause) maybe(match func(*pause) (what string, hit bool)) {
	p.mu.Lock()
	fire := false
	if p.armed && !p.fired {
		if what, hit := match(p); hit {
			fire, p.fired, p.what = true, true, what
		}
	}
	p.mu.Unlock()
	if fire {
		close(p.hit)
		<-p.release
	}
}

// resume lets the paused transaction go on. Safe to call more than once.
func (p *pause) resume() { p.once.Do(func() { close(p.release) }) }

// matched returns the point the pause fired at: the full text of the statement,
// or "anchor <table>/<id>"; "" until it fires. A race test asserts on it to
// prove it parked at the statement it meant, not at another statement that
// merely contains the armed text.
func (p *pause) matched() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.what
}

// pauseDialect is the PostgreSQL dialect with the pause hooked in.
type pauseDialect struct {
	sqlstore.Dialect

	p *pause
}

func (d pauseDialect) Rebind(q string) string {
	d.p.maybe(func(p *pause) (string, bool) { return q, p.stmt != "" && strings.Contains(q, p.stmt) })
	return d.Dialect.Rebind(q)
}

func (d pauseDialect) LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	d.p.maybe(func(p *pause) (string, bool) {
		for _, a := range anchors {
			if p.table != "" && a.Table == p.table && a.ID == p.id {
				return "anchor " + a.Table + "/" + a.ID, true
			}
		}
		return "", false
	})
	return d.Dialect.LockAnchors(ctx, tx, anchors...)
}

// pgRace opens a PostgreSQL store with a pause hook (skipped without
// PostgreSQL). The pause is released when the test ends, so a failed test
// never leaves a transaction parked.
//
// It skips under SQI_TEST_PG_FORCE_TX_RETRY=1. PostgresWith then rehearses
// every transaction once and rolls the rehearsal back, so the one-shot pause
// would fire in the rehearsal and the run that commits would never meet the
// forced interleaving: the test would pass without testing anything.
func pgRace(t *testing.T) (*postgres.Store, *pause) {
	t.Helper()
	if os.Getenv("SQI_TEST_PG_FORCE_TX_RETRY") == "1" {
		t.Skip("a forced interleaving cannot survive a rehearsed transaction: the pause would fire in the rolled-back run")
	}
	p := newPause()
	opts := postgres.DefaultOptions()
	opts.WrapDialect = func(d sqlstore.Dialect) sqlstore.Dialect { return pauseDialect{Dialect: d, p: p} }
	st := storetest.PostgresWith(t, opts)
	p.dsn = st.DSNForTest()
	t.Cleanup(p.resume)
	return st, p
}

// interleave runs paused until it reaches the armed point, then runs
// competitor, and watches until competitor either finishes or is seen waiting
// on a lock (a backend of the store's database, other than the watcher's,
// with wait_event_type 'Lock' in pg_stat_activity; the paused transaction is
// idle, not waiting). Then it resumes paused and waits for both.
// competitorWaited reports which came first: whether a lock made competitor
// wait. Watching the database rather than a timer means a competitor that is
// merely slow (a loaded runner) is not mistaken for one that waited.
//
// It fails the test at once if paused finishes without reaching the armed
// point (reporting paused's error), if the point is not reached within 10s, if
// competitor neither finishes nor waits on a lock within 30s, or if either
// operation is still running 30s after the resume.
func interleave(t *testing.T, p *pause, paused, competitor func() error) (pausedErr, competitorErr error, competitorWaited bool) {
	t.Helper()
	pdone := make(chan error, 1)
	go func() { pdone <- paused() }()
	select {
	case <-p.hit:
	case err := <-pdone:
		t.Fatalf("paused operation finished without reaching the pause point: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("pause point never reached: the operation did not run the armed statement or anchor dynamically")
	}
	waiting := lockWaitProbe(t, p.dsn)
	cdone := make(chan error, 1)
	go func() { cdone <- competitor() }()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case competitorErr = <-cdone:
			p.resume()
			return finish(t, pdone, "paused operation"), competitorErr, false
		case <-tick.C:
			if !waiting() {
				continue
			}
			p.resume()
			pausedErr = finish(t, pdone, "paused operation")
			return pausedErr, finish(t, cdone, "competitor"), true
		case <-deadline:
			p.resume()
			t.Fatal("competitor neither finished nor waited on a lock within 30s of the pause")
		}
	}
}

// lockWaitProbe returns a check, over its own connection to the database at
// dsn, of whether any other backend of that database is waiting on a lock.
func lockWaitProbe(t *testing.T, dsn string) func() bool {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open the lock-wait watcher: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the lock-wait watcher: %v", err)
		}
	})
	return func() bool {
		var waiting bool
		err := db.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1 FROM pg_stat_activity
	WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock')`).Scan(&waiting)
		if err != nil {
			t.Fatalf("watch for a lock wait: %v", err)
		}
		return waiting
	}
}

// finish waits for an operation interleave started, failing the test if it is
// still running 30s after the paused transaction resumed.
func finish(t *testing.T, done <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s still blocked 30s after the paused transaction resumed", name)
		return nil
	}
}

// TestPause_HoldsATransactionAtTheArmedStatement proves the hook works before
// any race test relies on it: a cancel paused at its task UPDATE holds the job
// row, so a second cancel of the same job waits for it.
func TestPause_HoldsATransactionAtTheArmedStatement(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1)
	p.atStatement(sqlstore.SQLCancelJobTasks)
	firstErr, secondErr, waited := interleave(t, p,
		func() error { _, err := st.CancelJobExecution(t.Context(), w.JobID, "first", time.Now()); return err },
		func() error { _, err := st.CancelJobExecution(t.Context(), w.JobID, "second", time.Now()); return err })
	if firstErr != nil || secondErr != nil {
		t.Fatalf("cancels: first %v, second %v", firstErr, secondErr)
	}
	if got := p.matched(); !strings.Contains(got, sqlstore.SQLCancelJobTasks) {
		t.Fatalf("paused at %q, want the task UPDATE", got)
	}
	if !waited {
		t.Fatal("the second cancel did not wait for the first's job-row lock")
	}
}

// TestPause_StopsBeforeTheArmedAnchorIsLocked proves an anchor point fires
// before the lock is taken: while a cancel is parked at its job anchor, a
// separate connection can lock the job row FOR UPDATE NOWAIT (which fails with
// lock_not_available if the cancel already holds it), and the resumed cancel
// then takes the lock and cancels the job's leased task.
func TestPause_StopsBeforeTheArmedAnchorIsLocked(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1"))
	raw, err := sql.Open("pgx", st.DSNForTest())
	if err != nil {
		t.Fatalf("open a separate connection: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close the separate connection: %v", err)
		}
	})
	p.atAnchor("jobs", w.JobID)
	cancelErr, lockErr, waited := interleave(t, p,
		func() error { _, err := st.CancelJobExecution(t.Context(), w.JobID, "cancel", time.Now()); return err },
		func() error {
			var one int
			return raw.QueryRowContext(t.Context(), `SELECT 1 FROM jobs WHERE id = $1 FOR UPDATE NOWAIT`, w.JobID).Scan(&one)
		})
	if cancelErr != nil {
		t.Fatalf("cancel: %v", cancelErr)
	}
	if lockErr != nil || waited {
		t.Fatalf("job row not free while the cancel was parked at its anchor (err %v, waited %v): the pause fired after the lock", lockErr, waited)
	}
	if got, want := p.matched(), "anchor jobs/"+w.JobID; got != want {
		t.Fatalf("paused at %q, want %q", got, want)
	}
	task, err := st.GetTask(t.Context(), w.Tasks[0])
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusCanceled {
		t.Fatalf("task status = %s, want canceled: the resumed cancel did not see the leased task", task.Status)
	}
}

// TestPause_ASlowCompetitorHasNotWaited proves interleave reads "waited" from
// the database, not from a timer: a competitor that takes longer than any
// settle time but waits on no lock (as a loaded runner can make one) is
// reported as not having waited.
func TestPause_ASlowCompetitorHasNotWaited(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1)
	p.atStatement(sqlstore.SQLCancelJobTasks)
	cancelErr, sleepErr, waited := interleave(t, p,
		func() error { _, err := st.CancelJobExecution(t.Context(), w.JobID, "cancel", time.Now()); return err },
		func() error { time.Sleep(1500 * time.Millisecond); return nil })
	if cancelErr != nil || sleepErr != nil {
		t.Fatalf("cancel: %v, competitor: %v", cancelErr, sleepErr)
	}
	if waited {
		t.Fatal("a competitor that only took its time was reported as having waited on a lock")
	}
}
