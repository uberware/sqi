// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// TestClassifySQLite_ConstraintCodes provokes each constraint SQLite reports
// and checks the SQLite dialect classifies it, and mapErr maps it, as a
// conflict; a syntax error stays unclassified and unmapped.
func TestClassifySQLite_ConstraintCodes(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir()+"/t.db", DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	insFarm := `INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, '', '')`
	if _, err := s.db.ExecContext(ctx, insFarm, "f1", "farm-one"); err != nil {
		t.Fatalf("seed farm: %v", err)
	}
	_, pk := s.db.ExecContext(ctx, insFarm, "f1", "farm-two")
	_, unique := s.db.ExecContext(ctx, insFarm, "f2", "farm-one")
	_, fk := s.db.ExecContext(ctx,
		`INSERT INTO queues (id, farm_id, name, created_at, updated_at) VALUES ('q1', 'no-such-farm', 'q', '', '')`)
	_, syntax := s.db.ExecContext(ctx, `INSERT INTO`)

	for name, c := range map[string]struct {
		err  error
		want ErrClass
	}{
		"primary key": {pk, ErrClassConflict},
		"unique":      {unique, ErrClassConflict},
		"foreign key": {fk, ErrClassConflict},
		"syntax":      {syntax, ErrClassOther},
		"no rows":     {sql.ErrNoRows, ErrClassNotFound},
	} {
		if c.err == nil {
			t.Fatalf("%s: the statement succeeded; it must fail for this test", name)
		}
		if got := (sqliteDialect{}).Classify(c.err); got != c.want {
			t.Errorf("%s: Classify(%v) = %d, want %d", name, c.err, got, c.want)
		}
	}
	for _, err := range []error{pk, unique, fk} {
		if !errors.Is(mapErr(classifyErr(sqliteDialect{}, err)), store.ErrConflict) {
			t.Errorf("mapErr(%v) is not ErrConflict", err)
		}
	}
	if got := mapErr(classifyErr(sqliteDialect{}, syntax)); !errors.Is(got, syntax) || errors.Is(got, store.ErrConflict) {
		t.Errorf("mapErr(syntax) = %v, want the error unchanged", got)
	}
}
