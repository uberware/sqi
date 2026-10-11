// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/sqlstore"
)

// TestClassifySQLite_ConstraintCodes provokes each constraint SQLite reports
// and checks the SQLite dialect classifies it, and mapErr maps it, as a
// conflict; a syntax error stays unclassified and unmapped.
func TestClassifySQLite_ConstraintCodes(t *testing.T) {
	s, err := sqlite.Open(t.Context(), t.TempDir()+"/t.db", sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d := s.Dialect() // the SQLite dialect
	ctx := t.Context()
	insFarm := `INSERT INTO farms (id, name, created_at, updated_at) VALUES (?, ?, '', '')`
	if _, err := s.WriteDB().ExecContext(ctx, insFarm, "f1", "farm-one"); err != nil {
		t.Fatalf("seed farm: %v", err)
	}
	_, pk := s.WriteDB().ExecContext(ctx, insFarm, "f1", "farm-two")
	_, unique := s.WriteDB().ExecContext(ctx, insFarm, "f2", "farm-one")
	_, fk := s.WriteDB().ExecContext(ctx,
		`INSERT INTO queues (id, farm_id, name, created_at, updated_at) VALUES ('q1', 'no-such-farm', 'q', '', '')`)
	_, syntax := s.WriteDB().ExecContext(ctx, `INSERT INTO`)

	for name, c := range map[string]struct {
		err  error
		want sqlstore.ErrClass
	}{
		"primary key": {pk, sqlstore.ErrClassConflict},
		"unique":      {unique, sqlstore.ErrClassConflict},
		"foreign key": {fk, sqlstore.ErrClassConflict},
		"syntax":      {syntax, sqlstore.ErrClassOther},
		"no rows":     {sql.ErrNoRows, sqlstore.ErrClassNotFound},
	} {
		if c.err == nil {
			t.Fatalf("%s: the statement succeeded; it must fail for this test", name)
		}
		if got := d.Classify(c.err); got != c.want {
			t.Errorf("%s: Classify(%v) = %d, want %d", name, c.err, got, c.want)
		}
	}
	for _, err := range []error{pk, unique, fk} {
		if !errors.Is(sqlstore.MapErr(sqlstore.ClassifyErr(d, err)), store.ErrConflict) {
			t.Errorf("mapErr(%v) is not ErrConflict", err)
		}
	}
	if got := sqlstore.MapErr(sqlstore.ClassifyErr(d, syntax)); !errors.Is(got, syntax) || errors.Is(got, store.ErrConflict) {
		t.Errorf("mapErr(syntax) = %v, want the error unchanged", got)
	}
}
