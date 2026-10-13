// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// TestPostgres_PingWhileLargeSubmission: readiness answers within the health
// budget while a 50,000-task submission holds a write connection.
func TestPostgres_PingWhileLargeSubmission(t *testing.T) {
	st := storetest.Postgres(t)
	farm, err := st.CreateFarm(t.Context(), store.Farm{ID: "f", Name: "f"})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := st.CreateQueue(t.Context(), store.Queue{ID: "q", FarmID: farm.ID, Name: "q"})
	if err != nil {
		t.Fatal(err)
	}
	seed := storetest.NewJob("big", farm.ID, queue.ID).Step("big-s", store.StepStatusReady)
	for i := range 50_000 {
		seed.Task(fmt.Sprintf("big-%d", i), "big-s", store.TaskStatusReady)
	}

	// The submission runs off the test goroutine, so it reports through a
	// channel rather than failing the test itself (FailNow is for the test's
	// own goroutine).
	submitted := make(chan error, 1)
	go func() {
		_, err := st.CreateJobSubmission(t.Context(), seed.Sub)
		submitted <- err
	}()
	var (
		pings   int
		slowest time.Duration
	)
	for {
		select {
		case err := <-submitted:
			if err != nil {
				t.Fatalf("CreateJobSubmission: %v", err)
			}
			if pings == 0 {
				t.Fatal("the submission finished before the first ping; the measurement saw nothing")
			}
			t.Logf("%d pings during the submission, slowest %s", pings, slowest)
			if counts, err := st.CountTasksByJob(t.Context(), "big"); err != nil || counts[store.TaskStatusReady] != 50_000 {
				t.Errorf("after the submission the job has %v tasks (err %v), want 50000 ready", counts, err)
			}
			return
		default:
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		start := time.Now()
		err := st.Ping(ctx)
		cancel()
		elapsed := time.Since(start)
		if err != nil {
			<-submitted // let the submission end before the store closes beneath it
			t.Fatalf("Ping during submission failed after %s: %v", elapsed, err)
		}
		slowest = max(slowest, elapsed)
		pings++
		time.Sleep(50 * time.Millisecond)
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
		wantKeyShare          bool   // whether FOR KEY SHARE NOWAIT (a foreign-key check's lock) is refused
	}{
		{name: "existing row, exclusive", anchors: []sqlstore.Anchor{farm}, wantUpdate: true, wantShare: true, wantKeyShare: true},
		{
			// A capacity anchor: exclusive against writers and other anchors, but
			// not against an insert that only references the row.
			name:       "existing row, no key update",
			anchors:    []sqlstore.Anchor{{Table: "farms", ID: "f", Mode: sqlstore.LockNoKeyUpdate}},
			wantUpdate: true, wantShare: true,
		},
		{
			name:       "existing row, shared",
			anchors:    []sqlstore.Anchor{{Table: "farms", ID: "f", Mode: sqlstore.LockShare}},
			wantUpdate: true,
		},
		{name: "missing row locks nothing", anchors: []sqlstore.Anchor{{Table: "jobs", ID: "nope", Mode: sqlstore.LockUpdate}}},
		{
			name:       "missing row beside an existing one",
			anchors:    []sqlstore.Anchor{{Table: "jobs", ID: "nope", Mode: sqlstore.LockUpdate}, farm},
			wantUpdate: true, wantShare: true, wantKeyShare: true,
		},
		{
			// A run of one table and mode is locked by one statement.
			name:       "a run of rows, one missing",
			anchors:    []sqlstore.Anchor{farm, {Table: "farms", ID: "g-missing", Mode: sqlstore.LockUpdate}},
			wantUpdate: true, wantShare: true, wantKeyShare: true,
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
			if got := held(t, "KEY SHARE"); got != tc.wantKeyShare {
				t.Errorf("FOR KEY SHARE NOWAIT refused = %v, want %v", got, tc.wantKeyShare)
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

func TestLockDatabase_ExcludesAServerAndIsReleasable(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	lock, err := postgres.LockDatabase(t.Context(), dsn, nil)
	if err != nil {
		t.Fatalf("LockDatabase: %v", err)
	}
	opts := postgres.DefaultOptions()
	opts.ServerLock = true
	if _, err := postgres.Open(t.Context(), dsn, opts); !errors.Is(err, postgres.ErrServerLockHeld) {
		t.Fatalf("server Open beside a held DatabaseLock = %v, want ErrServerLockHeld", err)
	}
	if _, err := postgres.LockDatabase(t.Context(), dsn, nil); !errors.Is(err, postgres.ErrServerLockHeld) {
		t.Fatalf("second LockDatabase = %v, want ErrServerLockHeld", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	st, err := postgres.Open(t.Context(), dsn, opts)
	if err != nil {
		t.Fatalf("server Open after Release: %v", err)
	}
	_ = st.Close()
}

func TestOpen_RefusesANewerSchema(t *testing.T) {
	dsn := storetest.PostgresDSN(t) // fresh, migrated
	db := openRaw(t, dsn)
	maxV, err := migrations.MaxVersion("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, maxV+1); err != nil {
		t.Fatal(err)
	}
	for _, auto := range []bool{true, false} {
		_, err := postgres.Open(t.Context(), dsn, postgres.Options{MaxConns: 2, AutoMigrate: auto, ServerLock: true})
		if !errors.Is(err, migrations.ErrSchemaTooNew) {
			t.Fatalf("Open(AutoMigrate=%v) = %v, want ErrSchemaTooNew", auto, err)
		}
	}
	// A refused Open must have released the server lock it took.
	lock, err := postgres.LockDatabase(t.Context(), dsn, nil)
	if err != nil {
		t.Fatalf("lock still held after a refused Open: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir() + "/b.dump"
	storetest.RequirePostgresTool(t, "pg_dump")
	if err := postgres.BackupDSN(t.Context(), dsn, out, nil); err != nil {
		t.Fatalf("BackupDSN on a newer schema: %v", err)
	}
}
