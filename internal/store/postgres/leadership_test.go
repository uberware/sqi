// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

// A lease operation is expected to answer well inside this, however busy the
// write pool is.
const leaseBudget = time.Second

// openHA opens a store with a dedicated lease pool over a fresh database and
// returns it with its DSN.
func openHA(t *testing.T, maxConns int) (*postgres.Store, string) {
	t.Helper()
	return openHAWith(t, postgres.Options{MaxConns: maxConns})
}

// openHAWith is openHA with further options; AutoMigrate and HA are set.
func openHAWith(t *testing.T, opts postgres.Options) (*postgres.Store, string) {
	t.Helper()
	dsn := storetest.PostgresDSN(t)
	opts.AutoMigrate, opts.HA = true, true
	st, err := postgres.Open(t.Context(), dsn, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dsn
}

// holdFencedShare starts a transaction on its own connection that does what a
// fenced transaction does at its check: it takes the server_epoch row FOR
// SHARE and keeps it until release is called. It stands in for a leader
// frozen between its check and its commit, a moment the store's own write
// fence passes through in microseconds and offers no hook to hold.
func holdFencedShare(t *testing.T, db *sql.DB) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	var one int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM server_epoch WHERE id = 1 FOR SHARE`).Scan(&one); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_, _ = conn.ExecContext(ctx, `COMMIT`) //nolint:errcheck // test cleanup
			_ = conn.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// lockFarmRow holds a row lock on farm id on a connection of its own, so any
// store write that updates that farm blocks until release.
func lockFarmRow(t *testing.T, db *sql.DB, id string) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE farms SET name = name WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`) //nolint:errcheck // test cleanup
			_ = conn.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// stallDialect holds the first statement whose text contains stmt for hold,
// after closing hit: a slow step inside an otherwise real store write.
type stallDialect struct {
	sqlstore.Dialect

	stmt string
	hold time.Duration
	once *sync.Once
	hit  chan struct{}
}

func (d stallDialect) Rebind(q string) string {
	if strings.Contains(q, d.stmt) {
		d.once.Do(func() {
			close(d.hit)
			time.Sleep(d.hold)
		})
	}
	return d.Dialect.Rebind(q)
}

// waitForLockWaiters blocks until at least n backends of the database are
// waiting on a lock, so a test knows its writers hold their connections.
func waitForLockWaiters(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := db.QueryRowContext(t.Context(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d backends are waiting on a lock", got, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// within runs fn under budget and fails the test when it does not return in
// time or returns an error.
func within(t *testing.T, budget time.Duration, what string, fn func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	start := time.Now()
	if err := fn(ctx); err != nil {
		t.Fatalf("%s after %v: %v", what, time.Since(start), err)
	}
}

func leaseRoundTrip(st *postgres.Store) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		epoch, ok, err := st.AcquireLeadership(ctx, "a", time.Hour)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("acquire refused")
		}
		if ok, err := st.RenewLeadership(ctx, "a", epoch, time.Hour); err != nil || !ok {
			return errors.Join(err, errors.New("renew refused"))
		}
		if _, err := st.GetLeadership(ctx); err != nil {
			return err
		}
		if err := st.UpsertServerMember(ctx, store.ServerMember{
			InstanceID: "i-1", Name: "n", Host: "h", Version: "v", GrantsFingerprint: "f", StartedAt: time.Now(),
		}); err != nil {
			return err
		}
		if _, _, err := st.ListServerMembers(ctx); err != nil {
			return err
		}
		return nil
	}
}

// TestLeaseOps_UseDedicatedPool: with every write connection held by a writer
// stuck on a row lock, the lease and membership methods still answer. Without
// HA the same methods share the write pool and wait.
func TestLeaseOps_UseDedicatedPool(t *testing.T) {
	const maxConns = 3
	st, dsn := openHA(t, maxConns)
	raw := openRaw(t, dsn)
	ctx := t.Context()

	f, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"})
	if err != nil {
		t.Fatal(err)
	}
	release := lockFarmRow(t, raw, f.ID)
	var wg sync.WaitGroup
	for range maxConns {
		upd := f
		upd.Description = "blocked"
		wg.Go(func() {
			_, _ = st.UpdateFarm(ctx, upd) //nolint:errcheck // released by the test's cleanup
		})
	}
	defer wg.Wait()
	defer release()
	waitForLockWaiters(t, raw, maxConns)

	// The write pool really is exhausted: another write waits.
	blocked, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := st.CreateFarm(blocked, store.Farm{ID: "f2", Name: "F2"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a write with the write pool exhausted = %v, want it to wait", err)
	}

	within(t, leaseBudget, "lease round trip with the write pool exhausted", leaseRoundTrip(st))
}

// TestLeaseRenew_NotBlockedByFencedWriters: a renewal touches only the lease
// row, so a busy leader's fenced writes never delay it: 32 writers committing
// through the fence in a loop, each taking the epoch row FOR SHARE at its
// check, and beside them one slow fenced write (a job delete holding its rows
// for 3s mid-transaction, as a large retention delete would) that then
// commits through the fence too.
func TestLeaseRenew_NotBlockedByFencedWriters(t *testing.T) {
	const (
		writers = 32
		hold    = 3 * time.Second
	)
	stall := stallDialect{stmt: "DELETE FROM steps", hold: hold, once: new(sync.Once), hit: make(chan struct{})}
	st, _ := openHAWith(t, postgres.Options{
		MaxConns:    16,
		WrapDialect: func(d sqlstore.Dialect) sqlstore.Dialect { stall.Dialect = d; return stall },
	})
	ctx := t.Context()

	epoch, ok, err := st.AcquireLeadership(ctx, "a", time.Hour)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	lead := store.WithLeaderEpoch(ctx, epoch)

	farms := make([]store.Farm, writers+1)
	for i := range farms {
		if farms[i], err = st.CreateFarm(ctx, store.Farm{ID: fmt.Sprintf("f%d", i), Name: fmt.Sprintf("F%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q", FarmID: farms[writers].ID, Name: "q"}); err != nil {
		t.Fatal(err)
	}
	job := storetest.NewJob("slow", farms[writers].ID, "q").Step("slow-step", store.StepStatusReady)
	for i := range 1000 {
		job.Task(fmt.Sprintf("slow-t%d", i), "slow-step", store.TaskStatusReady)
	}
	job.Submit(t, st)

	stop := make(chan struct{})
	var (
		wg     sync.WaitGroup
		writes atomic.Int64
	)
	for i := range writers {
		f := farms[i]
		wg.Go(func() {
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				f.Description = strconv.Itoa(n)
				if _, err := st.UpdateFarm(lead, f); err != nil {
					t.Errorf("fenced UpdateFarm %s: %v", f.ID, err)
					return
				}
				writes.Add(1)
			}
		})
	}
	defer wg.Wait()
	defer close(stop)

	slow := make(chan error, 1)
	go func() { slow <- st.DeleteJob(lead, "slow") }()
	select {
	case <-stall.hit: // the tasks are deleted, uncommitted, and the write sleeps
	case err := <-slow:
		t.Fatalf("the slow delete finished before its slow step: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the slow delete never reached its slow step")
	}

	for range 5 {
		within(t, leaseBudget, "renew beside fenced writers", func(ctx context.Context) error {
			ok, err := st.RenewLeadership(ctx, "a", epoch, time.Hour)
			if err == nil && !ok {
				err = errors.New("renew refused")
			}
			return err
		})
	}
	select {
	case err := <-slow:
		t.Fatalf("the slow write finished (%v) before the renewals did; they were not measured beside it", err)
	default:
	}
	if writes.Load() == 0 {
		t.Fatal("no fenced write committed while the renewals ran")
	}

	select {
	case err := <-slow:
		if err != nil {
			t.Fatalf("the slow fenced delete: %v, want it committed", err)
		}
	case <-time.After(hold + 30*time.Second):
		t.Fatal("the slow fenced delete never finished")
	}
	if _, err := st.GetJob(ctx, "slow"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job after the slow fenced delete: err = %v, want it deleted", err)
	}
}

// TestAcquireLeadership_WaitsForFencedCommitInProgress: a transaction that has
// reached its epoch check (holds server_epoch FOR SHARE) and not yet
// committed delays a takeover until it commits, which is what orders the old
// leader's last write before the new leader's first.
func TestAcquireLeadership_WaitsForFencedCommitInProgress(t *testing.T) {
	st, dsn := openHA(t, 4)
	raw := openRaw(t, dsn)
	ctx := t.Context()

	release := holdFencedShare(t, raw)
	type result struct {
		epoch int64
		ok    bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		epoch, ok, err := st.AcquireLeadership(ctx, "a", time.Hour)
		done <- result{epoch, ok, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("AcquireLeadership returned %+v while a fenced commit was in progress", r)
	case <-time.After(500 * time.Millisecond):
	}
	// The takeover's lease update has not committed: nobody holds it yet.
	if l, err := st.GetLeadership(ctx); err != nil || l.Holder != "" || l.Epoch != 0 {
		t.Fatalf("lease while the takeover waits = %+v, err=%v, want it unchanged", l, err)
	}

	release()
	select {
	case r := <-done:
		if r.err != nil || !r.ok || r.epoch != 1 {
			t.Fatalf("AcquireLeadership = %+v, want epoch 1", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AcquireLeadership still waiting after the fenced transaction committed")
	}
	var fence int64
	if err := raw.QueryRowContext(ctx, `SELECT epoch FROM server_epoch WHERE id = 1`).Scan(&fence); err != nil || fence != 1 {
		t.Fatalf("server_epoch = %d, err=%v, want 1", fence, err)
	}
}

// TestLeadership_WorksWithoutAnHAPool: Options.HA is what opens the dedicated
// pool; a store without it still serves the lease methods.
func TestLeadership_WorksWithoutAnHAPool(t *testing.T) {
	st := storetest.Postgres(t)
	within(t, 5*time.Second, "lease round trip on a store without HA", leaseRoundTrip(st))
}

// TestLeadership_ConcurrentAcquirersElectOne: many servers polling at
// once produce exactly one leader per epoch.
func TestLeadership_ConcurrentAcquirersElectOne(t *testing.T) {
	st, dsn := openHA(t, 8)
	ctx := t.Context()
	const n = 12
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for i := range n {
		id := string(rune('a' + i))
		wg.Go(func() {
			if _, ok, err := st.AcquireLeadership(ctx, id, time.Hour); err != nil {
				t.Errorf("%s: %v", id, err)
			} else if ok {
				mu.Lock()
				winners = append(winners, id)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("winners = %v, want exactly one", winners)
	}
	l, err := st.GetLeadership(ctx)
	if err != nil || l.Holder != winners[0] || l.Epoch != 1 {
		t.Fatalf("lease = %+v, err=%v, want %s at epoch 1", l, err, winners[0])
	}
	var fence int64
	if err := openRaw(t, dsn).QueryRowContext(ctx, `SELECT epoch FROM server_epoch`).Scan(&fence); err != nil || fence != l.Epoch {
		t.Fatalf("server_epoch = %d, err=%v, want the winning epoch %d", fence, err, l.Epoch)
	}
}

// TestOpen_HAClosesTheLeasePool: Close leaves no backend of the store connected,
// the lease pool's included.
func TestOpen_HAClosesTheLeasePool(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	st, err := postgres.Open(t.Context(), dsn, postgres.Options{MaxConns: 2, AutoMigrate: true, HA: true})
	if err != nil {
		t.Fatal(err)
	}
	raw := openRaw(t, dsn)
	if _, _, err := st.AcquireLeadership(t.Context(), "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := raw.QueryRowContext(t.Context(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND application_name = 'sqi-server'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() == 0 {
		t.Fatal("no sqi-server backends while the store is open")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for count() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d sqi-server backends still connected after Close", count())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
