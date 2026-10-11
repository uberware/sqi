// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlstore"
)

// TestMapErr_SQLiteConstraintCodes provokes each constraint SQLite reports and
// checks mapErr maps it, once classified, as a conflict; a syntax error stays
// unmapped. TestClassifySQLite_ConstraintCodes (internal/store/sqlite) checks
// the classification itself.
func TestMapErr_SQLiteConstraintCodes(t *testing.T) {
	s := openTestStoreWB(t)
	d := s.Dialect() // the SQLite dialect
	ctx := t.Context()
	if _, err := s.WriteDB().ExecContext(ctx, insertTxFarm, "f1", "farm-one"); err != nil {
		t.Fatalf("seed farm: %v", err)
	}
	_, pk := s.WriteDB().ExecContext(ctx, insertTxFarm, "f1", "farm-two")
	_, unique := s.WriteDB().ExecContext(ctx, insertTxFarm, "f2", "farm-one")
	_, fk := s.WriteDB().ExecContext(ctx,
		`INSERT INTO queues (id, farm_id, name, created_at, updated_at) VALUES ('q1', 'no-such-farm', 'q', '', '')`)
	_, syntax := s.WriteDB().ExecContext(ctx, `INSERT INTO`)

	for name, err := range map[string]error{"primary key": pk, "unique": unique, "foreign key": fk, "syntax": syntax} {
		if err == nil {
			t.Fatalf("%s: the statement succeeded; it must fail for this test", name)
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
