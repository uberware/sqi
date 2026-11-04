// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store/postgres"
	"github.com/uberware/sqi/internal/store/storetest"
)

func clockMicros(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRowContext(t.Context(), "SELECT "+postgres.ClockMicros()).Scan(&v); err != nil {
		t.Fatalf("ClockMicros expression: %v", err)
	}
	return v
}

func TestClockMicros_IsDatabaseNowInMicros(t *testing.T) {
	db := openRaw(t, storetest.PostgresDSN(t))
	got := clockMicros(t, db)
	diff := time.Duration(got-time.Now().UnixMicro()) * time.Microsecond
	if diff < -5*time.Second || diff > 5*time.Second {
		t.Fatalf("ClockMicros = %d, %s from time.Now().UnixMicro(); want within 5s", got, diff)
	}
}

// TestClockMicros_AdvancesWithinATransaction pins why the expression uses
// clock_timestamp(): now() is frozen at transaction start, which would make a
// lease read late in a long transaction look fresher than it is.
func TestClockMicros_AdvancesWithinATransaction(t *testing.T) {
	db := openRaw(t, storetest.PostgresDSN(t))
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; nothing to undo
	read := func() int64 {
		var v int64
		if err := tx.QueryRowContext(t.Context(), "SELECT "+postgres.ClockMicros()).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := read()
	time.Sleep(20 * time.Millisecond)
	if second := read(); second <= first {
		t.Fatalf("ClockMicros went %d then %d inside one transaction; want it to advance", first, second)
	}
}

func TestSeedRows_LeaseAndEpochStartAtZero(t *testing.T) {
	db := openRaw(t, storetest.PostgresDSN(t))
	var (
		holder  sql.NullString
		epoch   int64
		expires int64
		n       int64
	)
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM server_lease`).Scan(&n); err != nil || n != 1 {
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
	db := openRaw(t, storetest.PostgresDSN(t))
	for _, q := range []string{
		`INSERT INTO server_lease (id, holder, epoch, expires_us) VALUES (2, NULL, 0, 0)`,
		`INSERT INTO server_epoch (id, epoch) VALUES (2, 0)`,
	} {
		_, err := db.ExecContext(t.Context(), q)
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Errorf("%s = %v; want a check constraint violation", q, err)
		}
	}
}

func TestForShare(t *testing.T) {
	if got := postgres.ForShare(); got != " FOR SHARE" {
		t.Errorf("ForShare = %q, want %q", got, " FOR SHARE")
	}
}
