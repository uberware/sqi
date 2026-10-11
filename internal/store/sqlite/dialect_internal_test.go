// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// recordingDialect wraps the SQLite dialect and records every query and
// argument the wrappers pass through.
type recordingDialect struct {
	sqliteDialect

	queries []string
	times   int
	bools   int
}

func (d *recordingDialect) Rebind(q string) string { d.queries = append(d.queries, q); return q }
func (d *recordingDialect) BindTime(t time.Time) any {
	d.times++
	return d.sqliteDialect.BindTime(t)
}
func (d *recordingDialect) BindBool(b bool) any { d.bools++; return d.sqliteDialect.BindBool(b) }

func openWithDialect(t *testing.T, d Dialect) *Store {
	t.Helper()
	s, err := openWith(t.Context(), t.TempDir()+"/t.db", DefaultOptions(), d)
	if err != nil {
		t.Fatalf("openWith: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestWrappers_RebindAndBindEveryArgument(t *testing.T) {
	d := &recordingDialect{}
	s := openWithDialect(t, d)
	prepared := len(d.queries)
	if prepared == 0 {
		t.Fatal("no statement was rebound while preparing")
	}
	now := time.Now()
	if _, err := s.w.ExecContext(t.Context(),
		`INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`, "f1", "one", now, &now); err != nil {
		t.Fatalf("exec: %v", err)
	}
	var paused dbBool
	if err := s.r.QueryRowContext(t.Context(), `SELECT ?`, true).Scan(&paused); err != nil || !bool(paused) {
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
	s := openWithDialect(t, sqliteDialect{})
	ins := `INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, '', '') RETURNING id`
	var id string
	if err := s.w.QueryRowContext(t.Context(), ins, "f1", "one").Scan(&id); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := s.w.QueryRowContext(t.Context(), ins, "f1", "two").Scan(&id)
	if !errors.Is(mapErr(err), store.ErrConflict) {
		t.Fatalf("duplicate via RETURNING: mapErr = %v, want ErrConflict", mapErr(err))
	}
	_, err = s.w.ExecContext(t.Context(), strings.TrimSuffix(ins, " RETURNING id"), "f1", "three")
	if !errors.Is(mapErr(err), store.ErrConflict) {
		t.Fatalf("duplicate via Exec: mapErr = %v, want ErrConflict", mapErr(err))
	}
}
