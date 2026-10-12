// SPDX-License-Identifier: AGPL-3.0-or-later

package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/sqlstore"
)

type recordTB struct {
	testing.TB

	errs []string
}

func (r *recordTB) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

type noLockDialect struct{ sqlstore.Dialect }

func (noLockDialect) LockAnchors(context.Context, *sql.Tx, ...sqlstore.Anchor) error { return nil }

func TestLockOrderDialect_FlagsAnInversion(t *testing.T) {
	rec := &recordTB{TB: t}
	d := newLockOrderDialect(rec, noLockDialect{})
	lock := func(tx *sql.Tx, anchors ...sqlstore.Anchor) {
		t.Helper()
		if err := d.LockAnchors(t.Context(), tx, anchors...); err != nil {
			t.Fatalf("LockAnchors: %v", err)
		}
	}
	tx := new(sql.Tx)
	lock(tx, sqlstore.Anchor{Table: "jobs", ID: "j"})
	lock(tx, sqlstore.Anchor{Table: "workers", ID: "w"}) // a worker after a job: inverted
	lock(new(sql.Tx), sqlstore.Anchor{Table: "jobs", ID: "b"}, sqlstore.Anchor{Table: "jobs", ID: "a"})
	if len(rec.errs) != 2 {
		t.Fatalf("errors = %v, want the two inversions", rec.errs)
	}
}

// TestLockOrderDialect_ForgetsEndedTransactions pins that the checker does not
// keep a rank for every transaction it has seen: once a transaction is
// garbage, its entry goes.
func TestLockOrderDialect_ForgetsEndedTransactions(t *testing.T) {
	d := newLockOrderDialect(t, noLockDialect{})
	for range 100 {
		if err := d.LockAnchors(t.Context(), new(sql.Tx), sqlstore.Anchor{Table: "jobs", ID: "j"}); err != nil {
			t.Fatalf("LockAnchors: %v", err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for d.tracked() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d transactions still tracked after they became garbage", d.tracked())
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}
