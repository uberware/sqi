// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

// haServerOpts is what a server in an HA group opens its store with.
func haServerOpts() postgres.Options {
	return postgres.Options{MaxConns: 4, AutoMigrate: true, ServerLock: true, HA: true, LeaseTTL: 15 * time.Second}
}

func nonHAServerOpts() postgres.Options {
	return postgres.Options{MaxConns: 4, AutoMigrate: true, ServerLock: true}
}

func openStore(t *testing.T, dsn string, opts postgres.Options) *postgres.Store {
	t.Helper()
	st, err := postgres.Open(t.Context(), dsn, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestHALock_TwoHAServersCoexist(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	openStore(t, dsn, haServerOpts())
	openStore(t, dsn, haServerOpts())
	openStore(t, dsn, haServerOpts())
}

func TestHALock_HAAndNonHARefuseEachOther(t *testing.T) {
	defer postgres.SetHALockWaitForTest(500 * time.Millisecond)()

	t.Run("non-HA after HA", func(t *testing.T) {
		dsn := storetest.PostgresDSN(t)
		openStore(t, dsn, haServerOpts())
		_, err := postgres.Open(t.Context(), dsn, nonHAServerOpts())
		if !errors.Is(err, postgres.ErrServerLockHeld) {
			t.Fatalf("non-HA Open beside an HA server = %v, want ErrServerLockHeld", err)
		}
	})
	t.Run("HA after non-HA", func(t *testing.T) {
		dsn := storetest.PostgresDSN(t)
		openStore(t, dsn, nonHAServerOpts())
		start := time.Now()
		_, err := postgres.Open(t.Context(), dsn, haServerOpts())
		if !errors.Is(err, postgres.ErrServerLockHeld) {
			t.Fatalf("HA Open beside a non-HA server = %v, want ErrServerLockHeld", err)
		}
		if took := time.Since(start); took < 500*time.Millisecond {
			t.Fatalf("HA Open gave up after %v, before waiting out the lock", took)
		}
	})
	t.Run("non-HA beside non-HA", func(t *testing.T) {
		dsn := storetest.PostgresDSN(t)
		openStore(t, dsn, nonHAServerOpts())
		start := time.Now()
		_, err := postgres.Open(t.Context(), dsn, nonHAServerOpts())
		if !errors.Is(err, postgres.ErrServerLockHeld) {
			t.Fatalf("second non-HA Open = %v, want ErrServerLockHeld", err)
		}
		if took := time.Since(start); took >= 500*time.Millisecond {
			t.Fatalf("non-HA Open waited %v; it must refuse at once", took)
		}
	})
}

func TestHALock_ErrorNamesBothHolders(t *testing.T) {
	msg := postgres.ErrServerLockHeld.Error()
	for _, want := range []string{"another sqi-server", "a command that needs the database alone", "0x7371692d73727672"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrServerLockHeld = %q, missing %q", msg, want)
		}
	}
}

func TestHALock_CommandsRefusedBesideHAServer(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	st := openStore(t, dsn, haServerOpts())
	if _, err := postgres.LockDatabase(t.Context(), dsn, nil); !errors.Is(err, postgres.ErrServerLockHeld) {
		t.Fatalf("LockDatabase beside an HA server = %v, want ErrServerLockHeld", err)
	}
	// Closing the last HA server frees the database for the command: the
	// shared lock is released, not merely left to the session's death.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := postgres.LockDatabase(t.Context(), dsn, nil)
	if err != nil {
		t.Fatalf("LockDatabase after the HA server closed: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestHALock_CommandHoldsServersOff(t *testing.T) {
	defer postgres.SetHALockWaitForTest(200 * time.Millisecond)()
	dsn := storetest.PostgresDSN(t)
	lock, err := postgres.LockDatabase(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Open(t.Context(), dsn, haServerOpts()); !errors.Is(err, postgres.ErrServerLockHeld) {
		t.Fatalf("HA Open beside a command = %v, want ErrServerLockHeld", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	openStore(t, dsn, haServerOpts())
}

// A server that holds the lock exclusive only while it migrates, or a command
// that is about to finish, must not make a starting HA server fail.
func TestHALock_WaitsForAnExclusiveHolderToFinish(t *testing.T) {
	defer postgres.SetHALockWaitForTest(10 * time.Second)()
	dsn := storetest.PostgresDSN(t)
	lock, err := postgres.LockDatabase(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		released <- lock.Release()
	}()
	start := time.Now()
	openStore(t, dsn, haServerOpts())
	if took := time.Since(start); took < 500*time.Millisecond {
		t.Fatalf("Open returned after %v, before the command released the lock", took)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}

func TestHALock_FirstServerMigrates(t *testing.T) {
	dsn := storetest.PostgresEmptyDSN(t)
	openStore(t, dsn, haServerOpts())
	want, err := migrations.MaxVersion("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := postgres.SchemaVersionDSN(t.Context(), dsn); err != nil || v != want {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, want)
	}
	// It dropped the exclusive lock for the shared one: a second server joins.
	openStore(t, dsn, haServerOpts())
}

// migrateTo brings an empty database to version v.
func migrateTo(t *testing.T, dsn string, v int64) {
	t.Helper()
	db := openRaw(t, dsn)
	p, err := migrations.NewProvider(db, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(t.Context(), v); err != nil {
		t.Fatalf("UpTo(%d): %v", v, err)
	}
}

// workerBrokerColumn is the column migration 00041 adds and every store
// statement over workers names. Open prepares those statements, so a database
// one migration behind can only be opened by a store if the column is already
// there: behindByOne gives a test that state, without migration 00041's other
// changes.
const workerBrokerColumn = "registration_broker_at"

// behindByOne brings an empty database to version 40, with workerBrokerColumn
// added by hand so that a store can be opened over it.
func behindByOne(t *testing.T, dsn string) {
	t.Helper()
	migrateTo(t, dsn, 40)
	if _, err := openRaw(t, dsn).ExecContext(t.Context(), `ALTER TABLE workers ADD COLUMN `+workerBrokerColumn+` ts`); err != nil {
		t.Fatal(err)
	}
}

func TestHALock_PendingMigrationWithOtherServerRefused(t *testing.T) {
	dsn := storetest.PostgresEmptyDSN(t)
	behindByOne(t, dsn)

	// The first server joins a database that is behind: it takes the lock
	// exclusive, so it is alone, and leaves the schema be (AutoMigrate off).
	first := haServerOpts()
	first.AutoMigrate = false
	st := openStore(t, dsn, first)

	_, err := postgres.Open(t.Context(), dsn, haServerOpts())
	if !errors.Is(err, postgres.ErrSchemaBehindWithOtherServers) {
		t.Fatalf("second HA Open on a database behind = %v, want ErrSchemaBehindWithOtherServers", err)
	}
	for _, want := range []string{"1 other sqi-server(s)", "stop them all to migrate", "migrate up"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if v, err := postgres.SchemaVersionDSN(t.Context(), dsn); err != nil || v != 40 {
		t.Fatalf("schema moved to %d (%v); the refused server must not migrate", v, err)
	}

	// The refused Open left no lock behind: once the other server stops, a
	// lone HA server migrates.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openRaw(t, dsn).ExecContext(t.Context(), `ALTER TABLE workers DROP COLUMN `+workerBrokerColumn); err != nil {
		t.Fatal(err)
	}
	openStore(t, dsn, haServerOpts())
	if v, err := postgres.SchemaVersionDSN(t.Context(), dsn); err != nil || v != 41 {
		t.Fatalf("SchemaVersion = %d, %v; want 41", v, err)
	}
}

// Without AutoMigrate the server never migrates, so a database that is behind
// is not this refusal's business (the store behaves as a non-HA one does).
func TestHALock_BehindDatabaseWithoutAutoMigrateJoins(t *testing.T) {
	dsn := storetest.PostgresEmptyDSN(t)
	behindByOne(t, dsn)
	opts := haServerOpts()
	opts.AutoMigrate = false
	openStore(t, dsn, opts)
	openStore(t, dsn, opts)
}

func TestHALock_NewerSchemaStillRefused(t *testing.T) {
	dsn := storetest.PostgresDSN(t)
	db := openRaw(t, dsn)
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (9999, true)`); err != nil {
		t.Fatal(err)
	}
	_, err := postgres.Open(t.Context(), dsn, haServerOpts())
	if !errors.Is(err, migrations.ErrSchemaTooNew) {
		t.Fatalf("HA Open on a newer schema = %v, want ErrSchemaTooNew", err)
	}
	// ... and left no lock behind.
	lock, err := postgres.LockDatabase(t.Context(), dsn, nil)
	if err != nil {
		t.Fatalf("the refused Open left the lock held: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

// show reads a session setting on a connection of db.
func show(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(t.Context(), `SHOW `+name).Scan(&v); err != nil {
		t.Fatalf("SHOW %s: %v", name, err)
	}
	return v
}

func TestHASession_TimeoutsSet(t *testing.T) {
	st := openStore(t, storetest.PostgresDSN(t), haServerOpts())
	pools := map[string]*sql.DB{"write": st.WriteDBForTest(), "read": st.ReadDBForTest(), "lease": st.LeaseDBForTest()}
	for name, db := range pools {
		if db == nil {
			t.Fatalf("%s pool is nil", name)
		}
		if got := show(t, db, "idle_in_transaction_session_timeout"); got != "5s" {
			t.Errorf("%s pool idle_in_transaction_session_timeout = %q, want 5s (lease_ttl 15s / 3)", name, got)
		}
		if got := show(t, db, "client_connection_check_interval"); got != "1s" {
			t.Errorf("%s pool client_connection_check_interval = %q, want 1s", name, got)
		}
		if got := show(t, db, "default_transaction_isolation"); got != "read committed" {
			t.Errorf("%s pool default_transaction_isolation = %q, want read committed", name, got)
		}
	}
}

func TestHASession_DefaultLeaseTTL(t *testing.T) {
	opts := haServerOpts()
	opts.LeaseTTL = 0
	st := openStore(t, storetest.PostgresDSN(t), opts)
	if got := show(t, st.WriteDBForTest(), "idle_in_transaction_session_timeout"); got != "5s" {
		t.Fatalf("idle_in_transaction_session_timeout = %q, want 5s (the default lease_ttl 15s / 3)", got)
	}
}

func TestHASession_IdleTransactionEnded(t *testing.T) {
	opts := haServerOpts()
	opts.LeaseTTL = 6 * time.Second // idle_in_transaction_session_timeout 2s
	st := openStore(t, storetest.PostgresDSN(t), opts)
	ctx := context.Background()

	// A transaction that takes a row and then goes silent, as a frozen leader's
	// does.
	tx, err := st.WriteDBForTest().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE server_epoch SET epoch = epoch`); err != nil {
		t.Fatal(err)
	}

	// Another session's write to that row waits for it, and proceeds once
	// PostgreSQL ends the idle session.
	raw := openRaw(t, st.DSNForTest())
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := raw.ExecContext(wctx, `UPDATE server_epoch SET epoch = epoch`); err != nil {
		t.Fatalf("waiting write: %v", err)
	}
	if took := time.Since(start); took < time.Second || took > 10*time.Second {
		t.Fatalf("waiting write took %v, want about the 2s idle timeout", took)
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1`); err == nil {
		t.Fatal("the idle transaction's session survived its timeout")
	}
	_ = tx.Rollback() //nolint:errcheck // the session is already gone
}

func TestNonHASession_NoTimeouts(t *testing.T) {
	st := openStore(t, storetest.PostgresDSN(t), nonHAServerOpts())
	for name, db := range map[string]*sql.DB{"write": st.WriteDBForTest(), "read": st.ReadDBForTest()} {
		for _, param := range []string{"idle_in_transaction_session_timeout", "client_connection_check_interval"} {
			if got := show(t, db, param); got != "0" {
				t.Errorf("non-HA %s pool %s = %q, want 0", name, param, got)
			}
		}
	}
	if st.LeaseDBForTest() != nil {
		t.Error("a non-HA store opened a lease pool")
	}
}
