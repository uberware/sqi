// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/sqlstore"
)

// recordingDialect wraps the SQLite dialect and records every query and
// argument the wrappers pass through, and the anchors each LockAnchors call
// is asked to lock.
type recordingDialect struct {
	sqlstore.Dialect

	queries []string
	times   int
	bools   int
	anchors [][]sqlstore.Anchor
}

func (d *recordingDialect) Rebind(q string) string { d.queries = append(d.queries, q); return q }
func (d *recordingDialect) BindTime(t time.Time) any {
	d.times++
	return d.Dialect.BindTime(t)
}
func (d *recordingDialect) BindBool(b bool) any { d.bools++; return d.Dialect.BindBool(b) }
func (d *recordingDialect) LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	d.anchors = append(d.anchors, slices.Clone(anchors))
	return d.Dialect.LockAnchors(ctx, tx, anchors...)
}

// withDialect returns a store over base's pools whose statements run through
// d. It is closed by t.Cleanup before base, which closes the pools again (a
// no-op).
func withDialect(t *testing.T, base *sqlite.Store, d sqlstore.Dialect) *sqlstore.Store {
	t.Helper()
	s, err := sqlstore.New(t.Context(), base.WriteDB(), base.ReadDB(), d)
	if err != nil {
		t.Fatalf("sqlstore.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestWrappers_RebindAndBindEveryArgument(t *testing.T) {
	base := openTestStoreWB(t)
	d := &recordingDialect{Dialect: base.Dialect()}
	s := withDialect(t, base, d)
	prepared := len(d.queries)
	if prepared == 0 || prepared != s.StmtCount() {
		t.Fatalf("rebound %d queries while preparing %d statements; want one Rebind per statement",
			prepared, s.StmtCount())
	}
	now := time.Now()
	if _, err := s.WritePool().ExecContext(t.Context(),
		`INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`, "f1", "one", now, &now); err != nil {
		t.Fatalf("exec: %v", err)
	}
	var paused sqlstore.DBBool
	if err := s.ReadPool().QueryRowContext(t.Context(), `SELECT ?`, true).Scan(&paused); err != nil || !bool(paused) {
		t.Fatalf("bool round trip = %v, %v", bool(paused), err)
	}
	if len(d.queries) != prepared+2 || d.times != 2 || d.bools != 1 {
		t.Fatalf("rebound %d ad hoc queries, bound %d times and %d bools; want 2, 2, 1",
			len(d.queries)-prepared, d.times, d.bools)
	}
}

// TestWrappers_ClassifyAtScan checks that a constraint error reported by
// Row.Scan (INSERT … RETURNING) is classified like one reported by Exec.
func TestWrappers_ClassifyAtScan(t *testing.T) {
	s := openTestStoreWB(t).Store
	ins := `INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, '', '') RETURNING id`
	var id string
	if err := s.WritePool().QueryRowContext(t.Context(), ins, "f1", "one").Scan(&id); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := s.WritePool().QueryRowContext(t.Context(), ins, "f1", "two").Scan(&id)
	if !errors.Is(sqlstore.MapErr(err), store.ErrConflict) {
		t.Fatalf("duplicate via RETURNING: mapErr = %v, want ErrConflict", sqlstore.MapErr(err))
	}
	_, err = s.WritePool().ExecContext(t.Context(), strings.TrimSuffix(ins, " RETURNING id"), "f1", "three")
	if !errors.Is(sqlstore.MapErr(err), store.ErrConflict) {
		t.Fatalf("duplicate via Exec: mapErr = %v, want ErrConflict", sqlstore.MapErr(err))
	}
}
