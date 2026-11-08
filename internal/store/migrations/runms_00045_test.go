// SPDX-License-Identifier: AGPL-3.0-or-later

package migrations_test

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/uberware/sqi/internal/store/storetest"
)

// TestMigration00045_BackfillsRunMs seeds every run-time case, rewinds past
// the tasks.run_ms column and migrates up: the backfill must store each
// succeeded task's latest-attempt duration exactly as a completion would, and
// NULL for every task it cannot measure.
func TestMigration00045_BackfillsRunMs(t *testing.T) {
	path, s, _ := openSeedable(t)
	want := storetest.SeedRunTimes(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := rewindTo(t, path, 44)
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("goose.Up: %v", err)
	}
	for id, w := range want {
		var got sql.NullInt64
		if err := db.QueryRowContext(t.Context(), `SELECT run_ms FROM tasks WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		checkRunMs(t, id, got, w)
	}
}

func checkRunMs(t *testing.T, id string, got sql.NullInt64, want *int64) {
	t.Helper()
	switch {
	case want == nil && got.Valid:
		t.Errorf("task %s run_ms = %d, want NULL", id, got.Int64)
	case want != nil && (!got.Valid || got.Int64 != *want):
		t.Errorf("task %s run_ms = %v, want %d", id, got, *want)
	}
}
