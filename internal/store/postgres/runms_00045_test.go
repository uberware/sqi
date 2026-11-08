// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestMigration00045_BackfillsRunMs is the PostgreSQL leg of the migrations
// package's test of the same name: every run-time case, rewound past the
// tasks.run_ms column and migrated up, holds the duration a completion would
// have stored, or NULL.
func TestMigration00045_BackfillsRunMs(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	st, err := postgres.Open(t.Context(), dsn, postgres.DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := storetest.SeedRunTimes(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openRaw(t, dsn)
	p, err := migrations.NewProvider(db, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 44); err != nil {
		t.Fatalf("DownTo(44): %v", err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for id, w := range want {
		var got sql.NullInt64
		if err := db.QueryRowContext(t.Context(), `SELECT run_ms FROM tasks WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		switch {
		case w == nil && got.Valid:
			t.Errorf("task %s run_ms = %d, want NULL", id, got.Int64)
		case w != nil && (!got.Valid || got.Int64 != *w):
			t.Errorf("task %s run_ms = %v, want %d", id, got, *w)
		}
	}
}
