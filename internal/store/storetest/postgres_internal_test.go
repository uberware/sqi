// SPDX-License-Identifier: AGPL-3.0-or-later

package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

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
	d := &lockOrderDialect{Dialect: noLockDialect{}, t: rec, last: map[*sql.Tx]int{}}
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
