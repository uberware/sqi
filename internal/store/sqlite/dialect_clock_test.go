// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/migrations"
)

func migratedRaw(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := migrations.NewProvider(db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestClockMicros_IsDatabaseNowInMicros(t *testing.T) {
	db := migratedRaw(t)
	var got int64
	if err := db.QueryRowContext(t.Context(), "SELECT "+(sqliteDialect{}).ClockMicros()).Scan(&got); err != nil {
		t.Fatalf("ClockMicros expression: %v", err)
	}
	diff := time.Duration(got-time.Now().UnixMicro()) * time.Microsecond
	if diff < -5*time.Second || diff > 5*time.Second {
		t.Fatalf("ClockMicros = %d, %s from time.Now().UnixMicro(); want within 5s", got, diff)
	}
}

func TestClockMicros_AdvancesBetweenStatements(t *testing.T) {
	db := migratedRaw(t)
	read := func() int64 {
		var v int64
		if err := db.QueryRowContext(t.Context(), "SELECT "+(sqliteDialect{}).ClockMicros()).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := read()
	time.Sleep(20 * time.Millisecond)
	if second := read(); second <= first {
		t.Fatalf("ClockMicros went %d then %d; want it to advance", first, second)
	}
}

func TestSeedRows_LeaseAndEpochStartAtZero(t *testing.T) {
	db := migratedRaw(t)
	var (
		holder  sql.NullString
		epoch   int64
		expires int64
		n       int64
	)
	if err := db.QueryRowContext(t.Context(), `SELECT count(*), max(epoch) FROM server_lease`).Scan(&n, &epoch); err != nil || n != 1 {
		t.Fatalf("server_lease holds %d rows, %v; want exactly the seed row", n, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT holder, epoch, expires_us FROM server_lease WHERE id = 1`).
		Scan(&holder, &epoch, &expires); err != nil {
		t.Fatal(err)
	}
	if holder.Valid || epoch != 0 || expires != 0 {
		t.Errorf("server_lease seed = holder %+v, epoch %d, expires_us %d; want NULL, 0, 0", holder, epoch, expires)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT count(*), max(epoch) FROM server_epoch`).Scan(&n, &epoch); err != nil || n != 1 || epoch != 0 {
		t.Errorf("server_epoch = %d rows, epoch %d, %v; want one row at epoch 0", n, epoch, err)
	}
}

func TestServerHA_SingletonTablesRefuseASecondRow(t *testing.T) {
	db := migratedRaw(t)
	for _, q := range []string{
		`INSERT INTO server_lease (id, holder, epoch, expires_us) VALUES (2, NULL, 0, 0)`,
		`INSERT INTO server_epoch (id, epoch) VALUES (2, 0)`,
	} {
		_, err := db.ExecContext(t.Context(), q)
		if err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s = %v; want a CHECK constraint failure", q, err)
		}
	}
}

func TestSQLiteDialect_ForShare(t *testing.T) {
	if got := (sqliteDialect{}).ForShare(); got != "" {
		t.Errorf("ForShare = %q, want \"\": SQLite has no row locks", got)
	}
}
