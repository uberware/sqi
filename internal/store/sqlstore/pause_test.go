// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"context"
	"database/sql"
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

// maybe blocks the caller if cond holds and the pause is armed and unfired.
func (p *pause) maybe(cond func(*pause) bool) {
	p.mu.Lock()
	fire := p.armed && !p.fired && cond(p)
	if fire {
		p.fired = true
	}
	p.mu.Unlock()
	if fire {
		close(p.hit)
		<-p.release
	}
}

// resume lets the paused transaction go on. Safe to call more than once.
func (p *pause) resume() { p.once.Do(func() { close(p.release) }) }

func (p *pause) waitHit(t *testing.T) {
	t.Helper()
	select {
	case <-p.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("pause point never reached: the operation did not run the armed statement or anchor dynamically")
	}
}

// pauseDialect is the PostgreSQL dialect with the pause hooked in.
type pauseDialect struct {
	sqlstore.Dialect

	p *pause
}

func (d pauseDialect) Rebind(q string) string {
	d.p.maybe(func(p *pause) bool { return p.stmt != "" && strings.Contains(q, p.stmt) })
	return d.Dialect.Rebind(q)
}

func (d pauseDialect) LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	d.p.maybe(func(p *pause) bool {
		for _, a := range anchors {
			if p.table != "" && a.Table == p.table && a.ID == p.id {
				return true
			}
		}
		return false
	})
	return d.Dialect.LockAnchors(ctx, tx, anchors...)
}

// pgRace opens a PostgreSQL store with a pause hook (skipped without
// PostgreSQL). The pause is released when the test ends, so a failed test
// never leaves a transaction parked.
func pgRace(t *testing.T) (*postgres.Store, *pause) {
	t.Helper()
	p := newPause()
	opts := postgres.DefaultOptions()
	opts.WrapDialect = func(d sqlstore.Dialect) sqlstore.Dialect { return pauseDialect{Dialect: d, p: p} }
	st := storetest.PostgresWith(t, opts)
	t.Cleanup(p.resume)
	return st, p
}

// interleave runs paused until it reaches the armed point, then runs
// competitor; it waits up to settle for competitor to finish (it will not if
// it is waiting on a lock the paused transaction holds), resumes paused, and
// waits for both. competitorWaited reports whether competitor was still
// running when paused resumed, that is, whether a lock made it wait.
func interleave(t *testing.T, p *pause, paused, competitor func() error) (pausedErr, competitorErr error, competitorWaited bool) {
	t.Helper()
	const settle = 750 * time.Millisecond
	pdone := make(chan error, 1)
	go func() { pdone <- paused() }()
	p.waitHit(t)
	cdone := make(chan error, 1)
	go func() { cdone <- competitor() }()
	select {
	case competitorErr = <-cdone:
		p.resume()
		return <-pdone, competitorErr, false
	case <-time.After(settle):
	}
	p.resume()
	pausedErr = <-pdone
	select {
	case competitorErr = <-cdone:
	case <-time.After(30 * time.Second):
		t.Fatal("competitor still blocked 30s after the paused transaction resumed")
	}
	return pausedErr, competitorErr, true
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
	if !waited {
		t.Fatal("the second cancel did not wait for the first's job-row lock")
	}
}

// TestPause_StopsBeforeTheArmedAnchorIsLocked proves an anchor point fires
// before the lock is taken: a cancel paused at its job anchor holds nothing
// yet, so a lease of the job's task (which does not anchor the job) goes
// through without waiting, and the resumed cancel then cancels the task the
// lease assigned.
func TestPause_StopsBeforeTheArmedAnchorIsLocked(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	p.atAnchor("jobs", w.JobID)
	var lease store.LeaseResult
	cancelErr, leaseErr, waited := interleave(t, p,
		func() error { _, err := st.CancelJobExecution(t.Context(), w.JobID, "cancel", time.Now()); return err },
		func() (err error) { lease, err = st.LeaseTask(t.Context(), leaseReq(w.Tasks[0], "w1")); return err })
	if cancelErr != nil || leaseErr != nil {
		t.Fatalf("cancel %v, lease %v", cancelErr, leaseErr)
	}
	if waited {
		t.Fatal("the lease waited: the anchor pause fired after the job row was locked")
	}
	if lease.Outcome != store.LeaseLeased {
		t.Fatalf("lease outcome = %v, want leased", lease.Outcome)
	}
	task, err := st.GetTask(t.Context(), w.Tasks[0])
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusCanceled {
		t.Fatalf("task status = %s, want canceled: the resumed cancel did not see the leased task", task.Status)
	}
}
