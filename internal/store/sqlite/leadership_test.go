// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/sqlstore"
)

// leaseFixture is a migrated SQLite file with a store over it whose leadership
// methods run on a pool of their own, beside a write pool pinned to one
// connection exactly as Open pins it.
type leaseFixture struct {
	st    *sqlstore.Store
	write *sql.DB
	lease *sql.DB
}

func openLeaseFixture(t *testing.T, withLeasePool bool, opts ...sqlstore.Option) leaseFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lease.db")
	dsn, err := DSN(path)
	if err != nil {
		t.Fatal(err)
	}
	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	write.SetMaxOpenConns(1)
	if _, err := write.ExecContext(t.Context(), `PRAGMA journal_mode = WAL`); err != nil {
		t.Fatal(err)
	}
	p, err := migrations.NewProvider(write, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := leaseFixture{write: write}
	if withLeasePool {
		if f.lease, err = sql.Open("sqlite", dsn); err != nil {
			t.Fatal(err)
		}
		f.lease.SetMaxOpenConns(2)
		opts = append(opts, sqlstore.WithLeasePool(f.lease))
	}
	if f.st, err = sqlstore.New(t.Context(), write, write, sqliteDialect{}, opts...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.Close() })
	return f
}

// epochRow reads the fence epoch the way a fenced write would.
func (f leaseFixture) epochRow(t *testing.T) int64 {
	t.Helper()
	var e int64
	if err := f.write.QueryRowContext(t.Context(), `SELECT epoch FROM server_epoch WHERE id = 1`).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAcquireLeadership_PublishesTheEpochToTheFenceRow(t *testing.T) {
	f := openLeaseFixture(t, false)
	ctx := t.Context()
	if got := f.epochRow(t); got != 0 {
		t.Fatalf("seed fence epoch = %d, want 0", got)
	}
	if _, ok, err := f.st.AcquireLeadership(ctx, "a", 50*time.Millisecond); err != nil || !ok {
		t.Fatalf("a: ok=%v err=%v", ok, err)
	}
	if got := f.epochRow(t); got != 1 {
		t.Fatalf("fence epoch after a = %d, want 1", got)
	}
	// A refused acquire changes neither row.
	time.Sleep(10 * time.Millisecond)
	if _, ok, err := f.st.AcquireLeadership(ctx, "b", time.Hour); err != nil || ok {
		t.Fatalf("b while held: ok=%v err=%v", ok, err)
	}
	if got := f.epochRow(t); got != 1 {
		t.Fatalf("fence epoch after a refusal = %d, want 1", got)
	}
	time.Sleep(100 * time.Millisecond)
	if _, ok, err := f.st.AcquireLeadership(ctx, "b", time.Hour); err != nil || !ok {
		t.Fatalf("b after expiry: ok=%v err=%v", ok, err)
	}
	if got := f.epochRow(t); got != 2 {
		t.Fatalf("fence epoch after b = %d, want 2", got)
	}
}

func TestAcquireLeadership_ForcedRetryRehearsalLeavesNoEpochBehind(t *testing.T) {
	f := openLeaseFixture(t, true, sqlstore.WithForcedRetry())
	epoch, ok, err := f.st.AcquireLeadership(t.Context(), "a", time.Hour)
	if err != nil || !ok || epoch != 1 {
		t.Fatalf("AcquireLeadership = (%d, %v, %v), want (1, true, nil): the rolled-back rehearsal must not count", epoch, ok, err)
	}
	if got := f.epochRow(t); got != 1 {
		t.Fatalf("fence epoch = %d, want 1", got)
	}
}

// TestLeadership_RunsOnTheLeasePool pins that the lease and membership
// methods do not queue behind a write pool whose every connection is taken.
func TestLeadership_RunsOnTheLeasePool(t *testing.T) {
	f := openLeaseFixture(t, true)
	ctx := t.Context()
	held, err := f.write.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	got := withinASecond(t, func(ctx context.Context) error {
		epoch, ok, err := f.st.AcquireLeadership(ctx, "a", time.Hour)
		if err != nil || !ok {
			return errors.Join(err, errors.New("acquire refused"))
		}
		if ok, err := f.st.RenewLeadership(ctx, "a", epoch, time.Hour); err != nil || !ok {
			return errors.Join(err, errors.New("renew refused"))
		}
		if _, err := f.st.GetLeadership(ctx); err != nil {
			return err
		}
		if _, _, err := f.st.ListServerMembers(ctx); err != nil {
			return err
		}
		if _, err := f.st.DeleteServerMembersUnseenFor(ctx, time.Hour); err != nil {
			return err
		}
		return f.st.ReleaseLeadership(ctx, "a", epoch)
	})
	if got != nil {
		t.Fatalf("leadership methods with the write pool exhausted: %v", got)
	}
}

// TestLeadership_ShareTheWritePoolWithoutALeasePool is the control for the
// test above: with no lease pool the same call does wait on the write pool,
// so the pass above proves the routing and not an idle pool.
func TestLeadership_ShareTheWritePoolWithoutALeasePool(t *testing.T) {
	f := openLeaseFixture(t, false)
	held, err := f.write.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := f.st.AcquireLeadership(ctx, "a", time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireLeadership = %v, want it to wait on the exhausted write pool", err)
	}
}

func TestClose_ClosesTheLeasePool(t *testing.T) {
	f := openLeaseFixture(t, true)
	if err := f.st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.lease.PingContext(t.Context()); err == nil {
		t.Fatal("lease pool still open after Close")
	}
	if err := f.st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// withinASecond runs fn under a one-second deadline and returns its error.
func withinASecond(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	return fn(ctx)
}
