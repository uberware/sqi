// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestOpen_MigratesToTheBaselineAndReapplyIsANoop(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	for range 2 { // the second open finds nothing to apply
		st, err := postgres.Open(t.Context(), dsn, postgres.DefaultOptions())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	db := openRaw(t, dsn)
	v, err := postgres.SchemaVersion(t.Context(), db)
	if err != nil || v != 37 {
		t.Fatalf("SchemaVersion = %d, %v; want 37", v, err)
	}
}

func TestMigrations_DownThenUp(t *testing.T) {
	db := openRaw(t, storetest.PostgresDSN(t))
	p, err := migrations.NewProvider(db, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Down(t.Context()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if v, err := postgres.SchemaVersion(t.Context(), db); err != nil || v != 0 {
		t.Fatalf("version after Down = %d, %v; want 0", v, err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up: %v", err)
	}
}

func TestOpen_SecondServerRefused(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	opts := postgres.DefaultOptions()
	opts.ServerLock = true
	first, err := postgres.Open(t.Context(), dsn, opts)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer first.Close()
	if _, err := postgres.Open(t.Context(), dsn, opts); !errors.Is(err, postgres.ErrServerLockHeld) {
		t.Fatalf("second server Open = %v, want ErrServerLockHeld", err)
	}
	cli, err := postgres.Open(t.Context(), dsn, postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatalf("a CLI open (no server lock) was refused: %v", err)
	}
	_ = cli.Close()
	_ = first.Close()
	again, err := postgres.Open(t.Context(), dsn, opts)
	if err != nil {
		t.Fatalf("Open after the first server closed: %v", err)
	}
	_ = again.Close()
}

func TestTimestampColumns_RejectAnyOtherForm(t *testing.T) {
	st := storetest.Postgres(t)
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "f", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	db := openRaw(t, st.DSNForTest())
	for _, bad := range []string{"2026-01-02T03:04:05Z", "2026-01-02 03:04:05", "2026-01-02T03:04:05.123Z"} {
		_, err := db.ExecContext(t.Context(), `UPDATE farms SET updated_at = $1 WHERE id = 'f'`, bad)
		if err == nil || !strings.Contains(err.Error(), "ts_check") {
			t.Errorf("updated_at = %q accepted (err %v); the ts domain CHECK must refuse it", bad, err)
		}
	}
}

func TestPing(t *testing.T) {
	if err := storetest.Postgres(t).Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestLockAnchors_LocksExistingRowsOnly(t *testing.T) {
	st := storetest.Postgres(t)
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "f", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	db := openRaw(t, st.DSNForTest())
	contender := openRaw(t, st.DSNForTest())
	farm := sqlstore.Anchor{Table: "farms", ID: "f", Mode: sqlstore.LockUpdate}

	// held reports whether a NOWAIT lock in mode on farm f is refused because
	// another transaction holds a conflicting lock (SQLSTATE 55P03).
	held := func(t *testing.T, mode string) bool {
		t.Helper()
		_, err := contender.ExecContext(t.Context(), `SELECT 1 FROM farms WHERE id = 'f' FOR `+mode+` NOWAIT`)
		if err == nil {
			return false
		}
		if pe, ok := errors.AsType[*pgconn.PgError](err); ok && pe.Code == "55P03" {
			return true
		}
		t.Fatalf("contender FOR %s: %v", mode, err)
		return false
	}
	begin := func(t *testing.T) *sql.Tx {
		t.Helper()
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	tests := []struct {
		name                  string
		anchors               []sqlstore.Anchor
		wantErr               string // substring; empty means no error
		wantUpdate, wantShare bool   // whether FOR UPDATE / FOR SHARE NOWAIT is then refused
	}{
		{name: "existing row, exclusive", anchors: []sqlstore.Anchor{farm}, wantUpdate: true, wantShare: true},
		{
			name:       "existing row, shared",
			anchors:    []sqlstore.Anchor{{Table: "farms", ID: "f", Mode: sqlstore.LockShare}},
			wantUpdate: true,
		},
		{name: "missing row locks nothing", anchors: []sqlstore.Anchor{{Table: "jobs", ID: "nope", Mode: sqlstore.LockUpdate}}},
		{
			name:       "missing row beside an existing one",
			anchors:    []sqlstore.Anchor{{Table: "jobs", ID: "nope", Mode: sqlstore.LockUpdate}, farm},
			wantUpdate: true, wantShare: true,
		},
		{
			name:    "disallowed table",
			anchors: []sqlstore.Anchor{{Table: "tasks", ID: "f", Mode: sqlstore.LockUpdate}},
			wantErr: "not allowed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := begin(t)
			// A deferred rollback, not a t.Cleanup: t.Context() is canceled
			// before cleanups run, and that would roll the transaction back
			// underneath it.
			defer func() {
				if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
					t.Errorf("rollback: %v", err)
				}
			}()
			err := postgres.LockAnchors(t.Context(), tx, tc.anchors...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LockAnchors = %v, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LockAnchors: %v", err)
			}
			if got := held(t, "UPDATE"); got != tc.wantUpdate {
				t.Errorf("FOR UPDATE NOWAIT refused = %v, want %v", got, tc.wantUpdate)
			}
			if got := held(t, "SHARE"); got != tc.wantShare {
				t.Errorf("FOR SHARE NOWAIT refused = %v, want %v", got, tc.wantShare)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if held(t, "UPDATE") {
				t.Error("the lock outlived its transaction")
			}
		})
	}
}

func openRaw(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}
